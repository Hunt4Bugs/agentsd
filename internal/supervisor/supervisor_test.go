package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/config"
	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
	"github.com/Hunt4Bugs/agentsd/internal/manifest"
	"github.com/Hunt4Bugs/agentsd/internal/runstore"
	"github.com/Hunt4Bugs/agentsd/internal/xdg"
)

type env struct {
	home string
	sup  *Supervisor
	cfg  *config.Config
	bin  string
}

func setup(t *testing.T) *env {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	dirs := xdg.FromEnv(func(string) string { return "" }, home)
	cfg := config.Default()
	cfg.Roots = []string{filepath.Join(home, "src"), filepath.Join(home, "wiki"), filepath.Join(home, "out")}
	for _, r := range cfg.Roots {
		os.MkdirAll(r, 0o755)
	}
	bin := filepath.Join(home, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\necho \"claude args: $*\"\nif [ \"$2\" = \"\" ]; then cat; fi\n"), 0o755)
	sup := New(&runstore.Store{Root: dirs.Runs()}, dirs, cfg, nil)
	sup.LookupEnv = func(k string) (string, bool) {
		switch k {
		case "PATH":
			return bin + ":/usr/bin:/bin", true
		case "HOME":
			return home, true
		case "SECRET_TOKEN", "API_KEY":
			return "hunter2", true
		}
		return "", false
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return &env{home: home, sup: sup, cfg: cfg, bin: bin}
}

func (e *env) execAgent(name, script string) *manifest.Resolved {
	return &manifest.Resolved{
		Name: name, Runtime: "exec", Command: []string{"/bin/sh", "-c", script},
		Read: []string{filepath.Join(e.home, "src")}, Write: []string{filepath.Join(e.home, "wiki")},
		Timeout: manifest.Duration(time.Minute), MaxConcurrent: 1, EnvPass: []string{"API_KEY"},
		EnvSet: map[string]string{}, OutRoot: filepath.Join(e.home, "out", name),
	}
}

func (e *env) runToEnd(t *testing.T, req StartRequest) *runstore.Run {
	t.Helper()
	r, err := e.sup.Start(req)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if done := e.sup.Done(r.ID); done != nil {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("run did not finish")
		}
	}
	final, err := e.sup.Store.Load(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return final
}

func TestExecSuccessEnvAndOutput(t *testing.T) {
	e := setup(t)
	a := e.execAgent("lint", `echo "prompt=$(cat)"; echo "out=$AGENTSD_OUT"; env > "$AGENTSD_OUT/env.txt"; echo warn >&2`)
	r := e.runToEnd(t, StartRequest{Agent: a, Prompt: "hi"})
	if r.Status != runstore.Succeeded || *r.ExitCode != 0 {
		t.Fatalf("%+v", r)
	}
	if r.Cwd != r.OutDir {
		t.Fatalf("default cwd should be out dir: %s", r.Cwd)
	}
	envTxt, err := os.ReadFile(filepath.Join(r.OutDir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(envTxt), "SECRET_TOKEN") || !strings.Contains(string(envTxt), "API_KEY=hunter2") {
		t.Fatalf("env: %s", envTxt)
	}
	if strings.Join(r.EnvPassed, ",") != "API_KEY" {
		t.Fatal(r.EnvPassed)
	}
	lines, _ := e.sup.Store.ReadLogs(r.ID, runstore.Stdout, runstore.Stderr)
	var got []string
	for _, l := range lines {
		got = append(got, l.Stream+":"+l.Text)
	}
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "stdout:prompt=hi") || !strings.Contains(joined, "stderr:warn") {
		t.Fatal(joined)
	}
	evs, _ := e.sup.Store.ReadEvents(r.ID)
	var types []string
	for _, ev := range evs {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "run.created,run.started,run.exited" {
		t.Fatal(types)
	}
	b, _ := os.ReadFile(filepath.Join(e.sup.Store.Dir(r.ID), runstore.RunFile))
	if strings.Contains(string(b), "hunter2") {
		t.Fatal("secret value recorded in run.json")
	}
}

func TestClaudeAdapterArgvAndStdin(t *testing.T) {
	e := setup(t)
	a := e.execAgent("res", "")
	a.Runtime, a.Command = "claude-code", nil
	r := e.runToEnd(t, StartRequest{Agent: a, Prompt: "what is up"})
	lines, _ := e.sup.Store.ReadLogs(r.ID, runstore.Stdout)
	if r.Status != runstore.Succeeded || len(lines) == 0 || lines[0].Text != "claude args: -p what is up" {
		t.Fatalf("%v %+v", r.Status, lines)
	}
	if strings.Join(r.Argv, " ") != "claude -p {prompt}" {
		t.Fatal(r.Argv)
	}
	r = e.runToEnd(t, StartRequest{Agent: a, Prompt: "from stdin", PromptStdin: true})
	lines, _ = e.sup.Store.ReadLogs(r.ID, runstore.Stdout)
	if len(lines) != 2 || lines[1].Text != "from stdin" {
		t.Fatalf("%+v", lines)
	}
}

func TestFailedExit(t *testing.T) {
	e := setup(t)
	r := e.runToEnd(t, StartRequest{Agent: e.execAgent("x", "exit 3")})
	if r.Status != runstore.Failed || *r.ExitCode != 3 {
		t.Fatalf("%+v", r)
	}
}

func TestTimeout(t *testing.T) {
	e := setup(t)
	start := time.Now()
	r := e.runToEnd(t, StartRequest{Agent: e.execAgent("x", "sleep 30"), Timeout: 300 * time.Millisecond})
	if r.Status != runstore.TimedOut || r.Timeout != "300ms" {
		t.Fatalf("%+v", r)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout took too long")
	}
	_, err := e.sup.Start(StartRequest{Agent: e.execAgent("x", "true"), Timeout: 2 * time.Minute})
	if exitcode.Of(err) != exitcode.Usage {
		t.Fatalf("raising timeout: %v", err)
	}
}

func TestStopAndConcurrency(t *testing.T) {
	e := setup(t)
	a := e.execAgent("x", "trap 'exit 0' TERM; sleep 30 & wait")
	r, err := e.sup.Start(StartRequest{Agent: a})
	if err != nil {
		t.Fatal(err)
	}
	rej, err := e.sup.Start(StartRequest{Agent: a})
	if exitcode.Of(err) != exitcode.Concurrency || rej.Status != runstore.Rejected {
		t.Fatalf("second run: %v %+v", err, rej)
	}
	if onDisk, _ := e.sup.Store.Load(rej.ID); onDisk == nil || onDisk.Status != runstore.Rejected {
		t.Fatal("rejected run not recorded")
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := e.sup.Stop(r.ID, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	<-e.sup.Done(r.ID)
	final, _ := e.sup.Store.Load(r.ID)
	if final.Status != runstore.Stopped {
		t.Fatalf("%+v", final)
	}
	if _, err := e.sup.Stop(r.ID, 0); exitcode.Of(err) != exitcode.Invalid {
		t.Fatalf("stop finished run: %v", err)
	}
}

func TestPreconditions(t *testing.T) {
	e := setup(t)
	a := e.execAgent("x", "true")
	_, err := e.sup.Start(StartRequest{Agent: a, Cwd: "/tmp"})
	if exitcode.Of(err) != exitcode.Invalid {
		t.Fatalf("cwd outside workspace: %v", err)
	}
	a.Command = []string{"definitely-not-a-binary-xyz"}
	_, err = e.sup.Start(StartRequest{Agent: a})
	if exitcode.Of(err) != exitcode.RuntimeUnavailable {
		t.Fatalf("missing runtime: %v", err)
	}
}

func TestPolicyViolation(t *testing.T) {
	e := setup(t)
	a := e.execAgent("x", `echo ok > "$AGENTSD_OUT/fine.txt"; echo ok > "$HOME/wiki/fine.md"; echo bad > "$HOME/src/bad.txt"`)
	r := e.runToEnd(t, StartRequest{Agent: a})
	if r.Violations != 1 {
		t.Fatalf("violations = %d", r.Violations)
	}
	evs, _ := e.sup.Store.ReadEvents(r.ID)
	last := evs[len(evs)-1]
	if last.Type != runstore.EvViolation || !strings.HasSuffix(last.Data["path"].(string), "src/bad.txt") {
		t.Fatalf("%+v", last)
	}
}

func TestSubscribeReplaysOutput(t *testing.T) {
	e := setup(t)
	r, err := e.sup.Start(StartRequest{Agent: e.execAgent("x", "echo early; sleep 0.3; echo late")})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	replay, ch, cancel, ok := e.sup.Subscribe(r.ID)
	if !ok {
		t.Fatal("not active")
	}
	defer cancel()
	var lines []string
	for _, ev := range replay {
		if ev.Type == runstore.EvOutput {
			lines = append(lines, ev.Data["line"].(string))
		}
	}
	for ev := range ch {
		if ev.Type == runstore.EvOutput {
			lines = append(lines, ev.Data["line"].(string))
		}
	}
	if strings.Join(lines, ",") != "early,late" {
		t.Fatal(lines)
	}
}

func TestMarkLostAndShutdown(t *testing.T) {
	e := setup(t)
	stale := &runstore.Run{ID: runstore.NewID(time.Now()), Agent: "x", Status: runstore.Running, DaemonPID: 1}
	e.sup.Store.Create(stale)
	if n, _ := e.sup.MarkLost(); n != 1 {
		t.Fatalf("marked %d", n)
	}
	got, _ := e.sup.Store.Load(stale.ID)
	if got.Status != runstore.Lost {
		t.Fatal(got.Status)
	}

	r, _ := e.sup.Start(StartRequest{Agent: e.execAgent("y", "sleep 30")})
	time.Sleep(100 * time.Millisecond)
	e.sup.Shutdown(time.Second)
	got, _ = e.sup.Store.Load(r.ID)
	if got.Status != runstore.Stopped {
		t.Fatal(got.Status)
	}
	if _, err := e.sup.Start(StartRequest{Agent: e.execAgent("z", "true")}); err == nil {
		t.Fatal("accepted run after shutdown")
	}
}
