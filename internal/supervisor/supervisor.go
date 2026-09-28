// Package supervisor starts, watches and stops agent runs.
package supervisor

import (
	"errors"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Hunt4Bugs/agentsd/internal/adapter"
	"github.com/Hunt4Bugs/agentsd/internal/config"
	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
	"github.com/Hunt4Bugs/agentsd/internal/manifest"
	"github.com/Hunt4Bugs/agentsd/internal/policy"
	"github.com/Hunt4Bugs/agentsd/internal/runstore"
	"github.com/Hunt4Bugs/agentsd/internal/xdg"
)

// Timeouts.
const (
	// TimeoutKillGrace is how long a timed-out run gets between SIGTERM and SIGKILL.
	TimeoutKillGrace = 10 * time.Second
	// DefaultStopGrace applies to `runs stop` and foreground Ctrl-C.
	DefaultStopGrace = 10 * time.Second
	// outputDrain bounds how long we wait for grandchildren holding the pipes.
	outputDrain = 5 * time.Second
)

type Supervisor struct {
	Store     *runstore.Store
	Dirs      xdg.Dirs
	Log       *slog.Logger
	DaemonPID int
	LookupEnv func(string) (string, bool)
	// ScanLimits override policy scan bounds (tests).
	ScanMaxTime time.Duration

	mu        sync.Mutex
	cfg       *config.Config
	accepting bool
	active    map[string]*proc
	wg        sync.WaitGroup
}

type proc struct {
	run      *runstore.Run
	agent    *manifest.Resolved
	cmd      *exec.Cmd
	sink     *Sink
	done     chan struct{}
	stopReq  bool
	timedOut bool
	killT    *time.Timer
	timeoutT *time.Timer
	// launchedAt is taken just before exec, so the policy scan window covers
	// writes made before StartedAt is recorded.
	launchedAt time.Time
}

func New(store *runstore.Store, dirs xdg.Dirs, cfg *config.Config, log *slog.Logger) *Supervisor {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Supervisor{Store: store, Dirs: dirs, Log: log, cfg: cfg, DaemonPID: os.Getpid(),
		LookupEnv: os.LookupEnv, accepting: true, active: map[string]*proc{}}
}

// SetConfig swaps the config used for new runs (reload).
func (s *Supervisor) SetConfig(cfg *config.Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

func (s *Supervisor) Config() *config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// StartRequest describes a run to start.
type StartRequest struct {
	Agent       *manifest.Resolved
	Prompt      string
	PromptStdin bool
	Cwd         string        // absolute; empty uses the manifest default
	Timeout     time.Duration // zero uses the manifest limit; may only lower it
	Labels      map[string]string
}

// Start validates and launches a run. A rejected run is still recorded and
// returned alongside the error.
func (s *Supervisor) Start(req StartRequest) (*runstore.Run, error) {
	a := req.Agent
	now := time.Now().UTC()
	id := runstore.NewID(now)
	r := &runstore.Run{
		ID: id, Agent: a.Name, Runtime: a.Runtime, Status: runstore.Pending,
		Prompt: req.Prompt, PromptStdin: req.PromptStdin, Labels: req.Labels,
		OutDir: filepath.Join(a.OutRoot, id), CreatedAt: now, DaemonPID: s.DaemonPID,
		EnvPassed: []string{}, Timeout: a.Timeout.String(),
	}
	timeout := time.Duration(a.Timeout)
	if req.Timeout > 0 {
		if req.Timeout > timeout {
			return nil, exitcode.New(exitcode.CodeUsage, "--timeout %s exceeds the agent's limit of %s (it may only be lowered)", req.Timeout, a.Timeout)
		}
		timeout = req.Timeout
		r.Timeout = manifest.Duration(timeout).String()
	}
	r.Cwd = req.Cwd
	if r.Cwd == "" {
		r.Cwd = a.Cwd
	}
	if r.Cwd == "" {
		r.Cwd = r.OutDir
	}

	s.mu.Lock()
	cfg := s.cfg
	rejErr := s.admitLocked(a, r, cfg)
	if rejErr != nil {
		s.mu.Unlock()
		return s.reject(r, rejErr)
	}
	if err := s.Store.Create(r); err != nil {
		s.mu.Unlock()
		return nil, exitcode.Wrap(exitcode.CodeInternal, err, "create run")
	}
	sink, err := openSink(s.Store, id)
	if err != nil {
		s.mu.Unlock()
		return nil, exitcode.Wrap(exitcode.CodeInternal, err, "open run files")
	}
	p := &proc{run: r, agent: a, sink: sink, done: make(chan struct{})}
	s.active[id] = p
	s.wg.Add(1)
	s.mu.Unlock()

	sink.Emit(runstore.NewEvent(id, runstore.EvCreated, now, createdData(r)))
	if err := s.launch(p, cfg, timeout); err != nil {
		s.finishFailedStart(p, err)
		return r, exitcode.Wrap(exitcode.CodeRunFailed, err, "start run")
	}
	return snapshot(p), nil
}

// admitLocked applies the hard preconditions: accepting, cwd policy (§8.1),
// runtime availability, and concurrency.
func (s *Supervisor) admitLocked(a *manifest.Resolved, r *runstore.Run, cfg *config.Config) *exitcode.E {
	if !s.accepting {
		return exitcode.New(exitcode.CodeDaemonUnreachable, "daemon is shutting down")
	}
	if r.Cwd != r.OutDir && !a.CwdAllowed(r.Cwd) {
		return exitcode.New(exitcode.CodePolicy, "cwd %s is outside agent %s's workspace.read ∪ workspace.write", r.Cwd, a.Name)
	}
	if _, err := adapter.Available(a, cfg); err != nil {
		return exitcode.Wrap(exitcode.CodeRuntimeUnavailable, err, "runtime unavailable")
	}
	perAgent := 0
	for _, p := range s.active {
		if p.agent.Name == a.Name {
			perAgent++
		}
	}
	if perAgent >= a.MaxConcurrent {
		return exitcode.New(exitcode.CodeConcurrency, "agent %s is at its max_concurrent limit (%d)", a.Name, a.MaxConcurrent)
	}
	if len(s.active) >= cfg.Daemon.MaxConcurrentRuns {
		return exitcode.New(exitcode.CodeConcurrency, "daemon is at max_concurrent_runs (%d)", cfg.Daemon.MaxConcurrentRuns)
	}
	return nil
}

func (s *Supervisor) reject(r *runstore.Run, e *exitcode.E) (*runstore.Run, error) {
	if e.Code == exitcode.CodeDaemonUnreachable {
		return nil, e
	}
	r.Status, r.Reason, r.ReasonCode = runstore.Rejected, e.Error(), e.Code
	ended := time.Now().UTC()
	r.EndedAt = &ended
	if err := s.Store.Create(r); err == nil {
		_ = s.Store.AppendEvent(runstore.NewEvent(r.ID, runstore.EvCreated, r.CreatedAt, createdData(r)))
	}
	s.Log.Info("run rejected", "run", r.ID, "agent", r.Agent, "reason", e.Error())
	return r, e
}

func (s *Supervisor) launch(p *proc, cfg *config.Config, timeout time.Duration) error {
	r, a := p.run, p.agent
	if err := os.MkdirAll(r.OutDir, 0o755); err != nil {
		return err
	}
	spec, err := adapter.Build(a, cfg, adapter.RunContext{
		RunID: r.ID, OutDir: r.OutDir, Cwd: r.Cwd, Prompt: r.Prompt, PromptStdin: r.PromptStdin,
		Dirs: s.Dirs, LookupEnv: s.LookupEnv,
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	r.Argv = redactPrompt(spec.Argv, r.Prompt)
	r.EnvPassed, r.EnvMissing = nonNil(spec.EnvPassed), spec.EnvMissing
	s.mu.Unlock()

	bin, err := lookPath(spec.Argv[0], spec.Env)
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, spec.Argv[1:]...)
	cmd.Args[0] = spec.Argv[0]
	cmd.Dir, cmd.Env = spec.Dir, spec.Env
	if spec.HasStdin {
		cmd.Stdin = strings.NewReader(spec.Stdin)
	}
	stdout, stderr := newLineWriter(runstore.Stdout, p.sink), newLineWriter(runstore.Stderr, p.sink)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = outputDrain
	p.launchedAt = policy.FSNow(s.Dirs.State())
	if err := cmd.Start(); err != nil {
		return err
	}
	p.cmd = cmd

	s.mu.Lock()
	started := time.Now().UTC()
	r.Status, r.StartedAt, r.PID, r.PGID = runstore.Running, &started, cmd.Process.Pid, cmd.Process.Pid
	_ = s.Store.Save(r)
	p.timeoutT = time.AfterFunc(timeout, func() { s.onTimeout(p) })
	s.mu.Unlock()
	p.sink.Emit(runstore.NewEvent(r.ID, runstore.EvStarted, started, map[string]any{"pid": r.PID, "argv0": spec.Argv[0]}))
	if len(spec.EnvMissing) > 0 {
		s.Log.Warn("env.pass variables not set in daemon environment", "run", r.ID, "names", spec.EnvMissing)
	}
	s.Log.Info("run started", "run", r.ID, "agent", a.Name, "pid", r.PID)

	go s.wait(p, stdout, stderr)
	return nil
}

// lookPath resolves argv0 against the PATH the child will see.
func lookPath(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, kv := range env {
		if p, ok := strings.CutPrefix(kv, "PATH="); ok {
			for _, dir := range filepath.SplitList(p) {
				c := filepath.Join(dir, name)
				if fi, err := os.Stat(c); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
					return c, nil
				}
			}
		}
	}
	return exec.LookPath(name)
}

func (s *Supervisor) wait(p *proc, stdout, stderr *lineWriter) {
	err := p.cmd.Wait()
	stdout.Finish()
	stderr.Finish()
	ended := time.Now().UTC()

	s.mu.Lock()
	p.timeoutT.Stop()
	if p.killT != nil {
		p.killT.Stop()
	}
	r := p.run
	r.EndedAt = &ended
	code, sig := exitInfo(p.cmd.ProcessState, err)
	r.ExitCode, r.Signal = code, sig
	switch {
	case p.timedOut:
		r.Status = runstore.TimedOut
	case p.stopReq:
		r.Status = runstore.Stopped
	case code != nil && *code == 0:
		r.Status = runstore.Succeeded
	default:
		r.Status = runstore.Failed
		if err != nil && !isExitError(err) {
			r.Reason = err.Error()
		}
	}
	scanIn := s.scanInputLocked(p)
	s.mu.Unlock()

	data := map[string]any{}
	if code != nil {
		data["exit_code"] = *code
	}
	if sig != "" {
		data["signal"] = sig
	}
	p.sink.Emit(runstore.NewEvent(r.ID, runstore.EvExited, ended, data))
	switch r.Status {
	case runstore.TimedOut:
		p.sink.Emit(runstore.NewEvent(r.ID, runstore.EvTimedOut, ended, map[string]any{"timeout": r.Timeout}))
	case runstore.Stopped:
		p.sink.Emit(runstore.NewEvent(r.ID, runstore.EvStopped, ended, nil))
	}

	res := policy.Scan(scanIn.ScanInput)
	concurrent := scanIn.concurrent
	for _, v := range res.Violations {
		p.sink.Emit(runstore.NewEvent(r.ID, runstore.EvViolation, time.Now(), map[string]any{
			"path": v.Path, "mtime": v.MTime.UTC().Format(runstore.EventFormat), "best_effort": true,
			"reason":          "created or modified outside workspace.write and AGENTSD_OUT during the run window",
			"concurrent_runs": concurrent, "scan_truncated": res.Truncated,
		}))
	}

	s.mu.Lock()
	r.Violations = len(res.Violations)
	r.PolicyScan = &runstore.ScanSummary{BestEffort: true, Entries: res.Entries, Truncated: res.Truncated,
		Duration: res.Elapsed.Round(time.Millisecond).String(), Roots: res.Roots}
	_ = s.Store.Save(r)
	delete(s.active, r.ID)
	keep := slices.Collect(maps.Keys(s.active))
	cfg := s.cfg
	s.mu.Unlock()
	p.sink.Close()
	close(p.done)
	s.wg.Done()
	s.Log.Info("run finished", "run", r.ID, "agent", r.Agent, "status", r.Status, "violations", r.Violations)
	if n, _ := s.Store.Prune(cfg.Runs.Retain, cfg.Runs.RetainDays, time.Now(), keep); n > 0 {
		s.Log.Info("pruned runs", "count", n)
	}
}

type scanInput struct {
	policy.ScanInput
	concurrent []string
}

func (s *Supervisor) scanInputLocked(p *proc) scanInput {
	in := scanInput{concurrent: []string{}}
	in.Roots = append(slices.Clone(s.cfg.Roots), p.run.Cwd)
	in.Allowed = append(slices.Clone(p.agent.Write), p.run.OutDir)
	in.Skip = []string{s.Dirs.Config(), s.Dirs.State(), s.Dirs.Cache(), s.Dirs.Data()}
	for id, o := range s.active {
		if id != p.run.ID {
			in.Skip = append(in.Skip, o.run.OutDir)
			in.concurrent = append(in.concurrent, id)
		}
	}
	in.Start, in.End = p.launchedAt, *p.run.EndedAt
	in.MaxTime = s.ScanMaxTime
	return in
}

func (s *Supervisor) finishFailedStart(p *proc, err error) {
	s.mu.Lock()
	ended := time.Now().UTC()
	p.run.Status, p.run.Reason, p.run.EndedAt = runstore.Failed, err.Error(), &ended
	_ = s.Store.Save(p.run)
	delete(s.active, p.run.ID)
	s.mu.Unlock()
	p.sink.Emit(runstore.NewEvent(p.run.ID, runstore.EvExited, ended, map[string]any{"error": err.Error()}))
	p.sink.Close()
	close(p.done)
	s.wg.Done()
	s.Log.Error("run failed to start", "run", p.run.ID, "err", err)
}

func (s *Supervisor) onTimeout(p *proc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.cmd == nil || p.run.Status.Terminal() || p.stopReq {
		return
	}
	p.timedOut = true
	s.terminateLocked(p, TimeoutKillGrace)
}

// terminateLocked sends SIGTERM to the process group, then SIGKILL after grace.
func (s *Supervisor) terminateLocked(p *proc, grace time.Duration) {
	pgid := p.cmd.Process.Pid
	if grace <= 0 {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if p.killT != nil {
		p.killT.Stop()
	}
	p.killT = time.AfterFunc(grace, func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
}

// Stop requests a stop: SIGTERM, then SIGKILL after grace.
func (s *Supervisor) Stop(id string, grace time.Duration) (*runstore.Run, error) {
	s.mu.Lock()
	p, ok := s.active[id]
	if !ok {
		s.mu.Unlock()
		r, err := s.Store.Load(id)
		if err != nil {
			return nil, exitcode.New(exitcode.CodeNotFound, "no run %s", id)
		}
		return r, exitcode.New(exitcode.CodeInvalid, "run %s is not active (status %s)", id, r.Status)
	}
	if p.cmd == nil {
		s.mu.Unlock()
		return nil, exitcode.New(exitcode.CodeInvalid, "run %s has not started yet", id)
	}
	p.stopReq = true
	s.terminateLocked(p, grace)
	r := *p.run
	s.mu.Unlock()
	s.Log.Info("run stop requested", "run", id, "grace", grace)
	return &r, nil
}

// Done returns a channel closed when the run finishes, or nil if not active.
func (s *Supervisor) Done(id string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.active[id]; ok {
		return p.done
	}
	return nil
}

// Subscribe follows an active run. ok is false when the run is not active.
func (s *Supervisor) Subscribe(id string) (replay []runstore.Event, ch <-chan runstore.Event, cancel func(), ok bool) {
	s.mu.Lock()
	p, found := s.active[id]
	s.mu.Unlock()
	if !found {
		return nil, nil, nil, false
	}
	replay, ch, cancel = p.sink.Subscribe()
	return replay, ch, cancel, true
}

// Get returns a copy of an active run's current state.
func (s *Supervisor) Get(id string) (*runstore.Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.active[id]; ok {
		r := *p.run
		return &r, true
	}
	return nil, false
}

// ActiveCount returns the number of active runs.
func (s *Supervisor) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// Broadcast emits an event (e.g. daemon.reload) into every active run.
func (s *Supervisor) Broadcast(typ string, data map[string]any) {
	s.mu.Lock()
	procs := slices.Collect(maps.Values(s.active))
	s.mu.Unlock()
	for _, p := range procs {
		p.sink.Emit(runstore.NewEvent(p.run.ID, typ, time.Now(), data))
	}
}

// Shutdown stops accepting runs, stops active runs (SIGTERM, then SIGKILL
// after grace) and waits for them to be recorded as stopped.
func (s *Supervisor) Shutdown(grace time.Duration) {
	s.mu.Lock()
	s.accepting = false
	for _, p := range s.active {
		if p.cmd != nil && !p.run.Status.Terminal() {
			p.stopReq = true
			s.terminateLocked(p, grace)
		}
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace + outputDrain + policy.DefaultMaxTime + 5*time.Second):
		s.Log.Error("timed out waiting for runs to finish during shutdown")
	}
}

// MarkLost marks runs left pending/running by a previous daemon as lost.
func (s *Supervisor) MarkLost() (int, error) {
	ids, err := s.Store.IDs()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		s.mu.Lock()
		_, mine := s.active[id]
		s.mu.Unlock()
		if mine {
			continue
		}
		r, err := s.Store.Load(id)
		if err != nil || r.Status.Terminal() {
			continue
		}
		now := time.Now().UTC()
		prev := r.DaemonPID
		r.Status, r.EndedAt = runstore.Lost, &now
		r.Reason = "the daemon exited while this run was active; its process (if still alive) was not killed"
		if err := s.Store.Save(r); err != nil {
			continue
		}
		_ = s.Store.AppendEvent(runstore.NewEvent(id, runstore.EvLost, now, map[string]any{"previous_daemon_pid": prev, "pid": r.PID, "pgid": r.PGID}))
		n++
	}
	return n, nil
}

func createdData(r *runstore.Run) map[string]any {
	d := map[string]any{"agent": r.Agent}
	if len(r.Labels) > 0 {
		d["labels"] = r.Labels
	}
	return d
}

func snapshot(p *proc) *runstore.Run {
	r := *p.run
	return &r
}

func exitInfo(ps *os.ProcessState, err error) (*int, string) {
	if ps == nil {
		return nil, ""
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return nil, unix.SignalName(ws.Signal())
	}
	c := ps.ExitCode()
	return &c, ""
}

func isExitError(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}

// redactPrompt replaces the prompt in argv so run.json doesn't store it twice.
func redactPrompt(argv []string, prompt string) []string {
	out := slices.Clone(argv)
	if prompt == "" {
		return out
	}
	for i, a := range out {
		if a == prompt {
			out[i] = "{prompt}"
		}
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
