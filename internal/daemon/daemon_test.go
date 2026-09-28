package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/api"
	"github.com/Hunt4Bugs/agentsd/internal/client"
	"github.com/Hunt4Bugs/agentsd/internal/env"
	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
	"github.com/Hunt4Bugs/agentsd/internal/xdg"
)

func testEnv(t *testing.T) *env.Env {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "adt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	dirs := xdg.FromEnv(func(string) string { return "" }, home)
	sock, fb := dirs.Socket()
	if fb {
		t.Fatal("test socket path too long")
	}
	return &env.Env{Home: home, Dirs: dirs, ConfigPath: filepath.Join(dirs.Config(), "config.toml"), SocketPath: sock}
}

func startDaemon(t *testing.T, e *env.Env) (cancel func(), done chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- Run(ctx, Options{Env: e, Grace: time.Second, Stderr: io.Discard}) }()
	c := client.New(e.SocketPath)
	for i := 0; i < 100; i++ {
		if _, err := c.Ping(context.Background()); err == nil {
			return cancel, done
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	t.Fatalf("daemon did not start: %v", <-done)
	return nil, nil
}

func TestDaemonSocketAndAPI(t *testing.T) {
	e := testEnv(t)
	cancel, done := startDaemon(t, e)

	fi, err := os.Stat(e.SocketPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", fi.Mode(), err)
	}
	if di, _ := os.Stat(filepath.Dir(e.SocketPath)); di.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir mode %v", di.Mode())
	}

	c := client.New(e.SocketPath)
	st, err := c.Ping(context.Background())
	if err != nil || !st.Daemon.Running || st.Daemon.PID != os.Getpid() {
		t.Fatalf("%+v %v", st, err)
	}
	_, err = c.StartRun(context.Background(), apiReq("ghost"))
	if exitcode.Of(err) != exitcode.NotFound {
		t.Fatalf("unregistered agent: %v", err)
	}

	// Raw error shape and unknown routes.
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", e.SocketPath)
	}}}
	resp, err := hc.Get("http://agentsd/v1/nope")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Error struct{ Code, Message string }
	}
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != 404 || body.Error.Code != "not_found" {
		t.Fatalf("%d %+v", resp.StatusCode, body)
	}

	// A second daemon refuses to steal the socket.
	err = Run(context.Background(), Options{Env: e, Grace: time.Second, Stderr: io.Discard})
	if err == nil {
		t.Fatal("second daemon started")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.SocketPath); !os.IsNotExist(err) {
		t.Fatal("socket not removed on shutdown")
	}
	if _, err := os.Stat(e.Dirs.PIDFile()); !os.IsNotExist(err) {
		t.Fatal("pid file not removed on shutdown")
	}
}

func TestPeerUIDCheck(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "pc")
	defer os.RemoveAll(dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, _ := net.Dial("unix", filepath.Join(dir, "s"))
		if c != nil {
			time.Sleep(100 * time.Millisecond)
			c.Close()
		}
	}()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := checkPeer(c, os.Getuid()); err != nil {
		t.Fatalf("own uid rejected: %v", err)
	}
	if err := checkPeer(c, os.Getuid()+1); err == nil {
		t.Fatal("foreign uid accepted")
	}
}

func apiReq(agent string) api.StartRunRequest { return api.StartRunRequest{Agent: agent} }
