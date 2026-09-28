package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/api"
	"github.com/Hunt4Bugs/agentsd/internal/env"
	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
	"github.com/Hunt4Bugs/agentsd/internal/runstore"
	"github.com/Hunt4Bugs/agentsd/internal/supervisor"
	"github.com/Hunt4Bugs/agentsd/internal/version"
)

func (d *Daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", d.handleStatus)
	mux.HandleFunc("POST /v1/reload", d.handleReload)
	mux.HandleFunc("GET /v1/agents", d.handleAgents)
	mux.HandleFunc("GET /v1/agents/{name}", d.handleAgent)
	mux.HandleFunc("POST /v1/runs", d.handleStartRun)
	mux.HandleFunc("GET /v1/runs", d.handleListRuns)
	mux.HandleFunc("GET /v1/runs/{id}", d.handleGetRun)
	mux.HandleFunc("POST /v1/runs/{id}/stop", d.handleStopRun)
	mux.HandleFunc("GET /v1/runs/{id}/events", d.handleEvents)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, exitcode.New(exitcode.CodeNotFound, "no route %s %s", r.Method, r.URL.Path))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, err error) { writeErrRun(w, err, "") }

func writeErrRun(w http.ResponseWriter, err error, runID string) {
	code := exitcode.CodeOf(err)
	writeJSON(w, api.HTTPStatus(code), api.ErrorBody{Error: api.ErrorDetail{Code: code, Message: err.Error(), RunID: runID}})
}

func decode(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		return exitcode.Wrap(exitcode.CodeUsage, err, "read body")
	}
	if len(body) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return exitcode.Wrap(exitcode.CodeUsage, err, "invalid JSON body")
	}
	return nil
}

func (d *Daemon) Status() api.Status {
	cfg, _ := d.state()
	agents := d.agentInfos()
	st := api.Status{Daemon: api.DaemonStatus{
		Running: true, PID: os.Getpid(), StartedAt: &d.started, Version: version.Version, Socket: d.env.SocketPath,
		Uptime: time.Since(d.started).Round(time.Second).String(),
	}}
	for _, a := range agents {
		if a.Registration == api.Registered || a.Registration == api.Changed {
			st.Agents.Defined++
			if a.Available {
				st.Agents.Available++
			}
		}
	}
	st.Runs, st.Policy = api.SummarizeRuns(d.store, time.Now())
	st.Runs.Running = d.sup.ActiveCount()
	st.Policy.Enforcement = cfg.Policy.Enforcement
	return st
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.Status())
}

func (d *Daemon) handleReload(w http.ResponseWriter, r *http.Request) {
	if err := d.reload(); err != nil {
		writeErr(w, err)
		return
	}
	_, l := d.state()
	writeJSON(w, http.StatusOK, map[string]any{"reloaded": true, "agents": len(l.Agents)})
}

// agentInfos reports what the daemon serves: registered agents, annotated
// with whether the manifest on disk has drifted.
func (d *Daemon) agentInfos() []api.Agent {
	cfg, l := d.state()
	manifests, _ := d.env.LoadAgents(cfg)
	return env.AgentInfos(cfg, manifests, l, d.store)
}

func (d *Daemon) handleAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"agents": d.agentInfos()})
}

func (d *Daemon) handleAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, a := range d.agentInfos() {
		if a.Name == name {
			writeJSON(w, http.StatusOK, a)
			return
		}
	}
	writeErr(w, exitcode.New(exitcode.CodeNotFound, "no agent %q", name))
}

func (d *Daemon) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var req api.StartRunRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	_, l := d.state()
	agent, ok := l.Agents[req.Agent]
	if !ok {
		writeErr(w, exitcode.New(exitcode.CodeNotFound, "agent %q is not registered with the daemon (run `agentsd apply`)", req.Agent))
		return
	}
	if agent.Runtime != "exec" && req.Prompt == "" {
		writeErr(w, exitcode.New(exitcode.CodeUsage, "agent %s (%s) needs a prompt", agent.Name, agent.Runtime))
		return
	}
	sr := supervisor.StartRequest{Agent: agent, Prompt: req.Prompt, PromptStdin: req.PromptStdin, Labels: req.Labels}
	if req.Cwd != "" {
		if !filepath.IsAbs(req.Cwd) {
			writeErr(w, exitcode.New(exitcode.CodeUsage, "cwd must be absolute"))
			return
		}
		sr.Cwd = filepath.Clean(req.Cwd)
	}
	if req.Timeout != "" {
		t, err := time.ParseDuration(req.Timeout)
		if err != nil || t <= 0 {
			writeErr(w, exitcode.New(exitcode.CodeUsage, "invalid timeout %q", req.Timeout))
			return
		}
		sr.Timeout = t
	}
	run, err := d.sup.Start(sr)
	if err != nil {
		id := ""
		if run != nil {
			id = run.ID
		}
		writeErrRun(w, err, id)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (d *Daemon) handleListRuns(w http.ResponseWriter, r *http.Request) {
	f, err := runstore.ParseFilter(r.URL.Query().Get("agent"), r.URL.Query().Get("status"), r.URL.Query().Get("since"), r.URL.Query().Get("limit"))
	if err != nil {
		writeErr(w, err)
		return
	}
	runs, err := d.store.List(f)
	if err != nil {
		writeErr(w, err)
		return
	}
	for i, run := range runs {
		if live, ok := d.sup.Get(run.ID); ok {
			runs[i] = live
		}
	}
	if runs == nil {
		runs = []*runstore.Run{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (d *Daemon) resolveRun(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := d.store.Resolve(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return "", false
	}
	return id, true
}

func (d *Daemon) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id, ok := d.resolveRun(w, r)
	if !ok {
		return
	}
	if live, ok := d.sup.Get(id); ok {
		writeJSON(w, http.StatusOK, live)
		return
	}
	run, err := d.store.Load(id)
	if err != nil {
		writeErr(w, exitcode.Wrap(exitcode.CodeNotFound, err, "load run"))
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (d *Daemon) handleStopRun(w http.ResponseWriter, r *http.Request) {
	id, ok := d.resolveRun(w, r)
	if !ok {
		return
	}
	var req api.StopRunRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	grace := supervisor.DefaultStopGrace
	if req.Grace != "" {
		g, err := time.ParseDuration(req.Grace)
		if err != nil || g < 0 {
			writeErr(w, exitcode.New(exitcode.CodeUsage, "invalid grace %q", req.Grace))
			return
		}
		grace = g
	}
	run, err := d.sup.Stop(id, grace)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

// handleEvents streams NDJSON. Without follow it returns persisted events.
// With follow it replays events and output, streams live events, and ends
// with stream.end (run finished) or stream.truncated (this follower fell
// behind; reconnect with after=<last seq>). after=N skips events up to seq N.
func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := d.resolveRun(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	follow := q.Get("follow") == "1" || q.Get("follow") == "true"
	var after int64
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, exitcode.New(exitcode.CodeUsage, "invalid after %q", v))
			return
		}
		after = n
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, _ := w.(http.Flusher)
	var last int64
	send := func(e runstore.Event) error {
		if e.Seq != 0 && e.Seq <= after {
			return nil
		}
		if _, err := w.Write(append(e.JSON(), '\n')); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		if e.Seq > last {
			last = e.Seq
		}
		return nil
	}
	end := func() {
		data := map[string]any{"last_seq": max(last, after)}
		if run, err := d.store.Load(id); err == nil {
			data["status"] = run.Status
		}
		_ = send(runstore.NewEvent(id, runstore.EvStreamEnd, time.Now(), data))
	}
	if !follow {
		evs, err := d.store.ReadEvents(id)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
		for _, e := range evs {
			_ = send(e)
		}
		return
	}
	replay, sub, active := d.sup.Subscribe(id)
	if !active {
		evs, err := d.store.Replay(id)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
		for _, e := range evs {
			if send(e) != nil {
				return
			}
		}
		end()
		return
	}
	defer sub.Cancel()
	w.WriteHeader(http.StatusOK)
	for _, e := range replay {
		if send(e) != nil {
			return
		}
	}
	for {
		select {
		case e, ok := <-sub.C():
			if !ok {
				if sub.Truncated() {
					_ = send(runstore.NewEvent(id, runstore.EvStreamTruncated, time.Now(), map[string]any{"last_seq": max(last, after)}))
					return
				}
				// The sink closes only after the final run.json is saved.
				end()
				return
			}
			if send(e) != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}
