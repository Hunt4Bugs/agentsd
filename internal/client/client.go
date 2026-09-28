// Package client talks to the daemon over its Unix socket.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/api"
	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
	"github.com/Hunt4Bugs/agentsd/internal/runstore"
)

type Client struct {
	Socket string
	hc     *http.Client
}

func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableCompression: true,
	}
	return &Client{Socket: socket, hc: &http.Client{Transport: tr}}
}

// APIError is an error body returned by the daemon.
type APIError struct {
	api.ErrorDetail
}

func (e *APIError) Error() string { return e.Message }

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agentsd"+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, exitcode.Wrap(exitcode.CodeDaemonUnreachable, err, fmt.Sprintf("daemon unreachable at %s (is it running? try `agentsd service start` or `agentsd daemon`)", c.Socket))
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var eb api.ErrorBody
		b, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(b, &eb) != nil || eb.Error.Code == "" {
			return nil, exitcode.New(exitcode.CodeInternal, "daemon returned %s", resp.Status)
		}
		return nil, &exitcode.E{Code: eb.Error.Code, Err: &APIError{eb.Error}}
	}
	return resp, nil
}

// Call performs a JSON request and decodes the response into out.
func (c *Client) Call(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Ping checks reachability quickly.
func (c *Client) Ping(ctx context.Context) (*api.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var st api.Status
	return &st, c.Call(ctx, http.MethodGet, "/v1/status", nil, &st)
}

func (c *Client) StartRun(ctx context.Context, req api.StartRunRequest) (*runstore.Run, error) {
	var r runstore.Run
	return &r, c.Call(ctx, http.MethodPost, "/v1/runs", req, &r)
}

func (c *Client) GetRun(ctx context.Context, id string) (*runstore.Run, error) {
	var r runstore.Run
	return &r, c.Call(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id), nil, &r)
}

func (c *Client) StopRun(ctx context.Context, id string, grace time.Duration) (*runstore.Run, error) {
	var r runstore.Run
	return &r, c.Call(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(id)+"/stop", api.StopRunRequest{Grace: grace.String()}, &r)
}

func (c *Client) Reload(ctx context.Context) error {
	return c.Call(ctx, http.MethodPost, "/v1/reload", nil, nil)
}

// Follow streams a run's events until the run ends, fn returns an error, or
// ctx is cancelled.
func (c *Client) Follow(ctx context.Context, id string, fn func(runstore.Event) error) error {
	resp, err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id)+"/events?follow=1", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var e runstore.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return sc.Err()
}
