//go:build e2e

// Package e2e drives the real binary against an isolated HOME. It maps onto
// the v0.1 acceptance checklist (spec §15). Run with `make e2e`.
package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agentsd-e2e-bin")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "agentsd")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/agentsd")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type sandbox struct {
	t      *testing.T
	home   string
	env    []string
	daemon *exec.Cmd
}

// newSandbox makes a short HOME (so the socket path fits without fallback)
// and a PATH with fake claude/codex runtimes.
func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "ae")
	if err != nil {
		t.Fatal(err)
	}
	home, _ = filepath.EvalSymlinks(home)
	t.Cleanup(func() { os.RemoveAll(home) })
	fake := filepath.Join(home, ".fakebin")
	os.MkdirAll(fake, 0o755)
	for _, name := range []string{"claude", "codex"} {
		script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo '" + name + " 9.9.9'; exit 0; fi\necho \"" + name + " $*\"\n"
		os.WriteFile(filepath.Join(fake, name), []byte(script), 0o755)
	}
	s := &sandbox{t: t, home: home}
	s.env = []string{"HOME=" + home, "PATH=" + fake + ":/usr/bin:/bin:/usr/sbin:/sbin", "USER=" + os.Getenv("USER"), "LANG=C", "NO_COLOR=1"}
	t.Cleanup(s.stopDaemon)
	return s
}

type result struct {
	code           int
	stdout, stderr string
}

func (s *sandbox) run(args ...string) result {
	return s.runIn(nil, args...)
}

func (s *sandbox) runIn(stdin []byte, args ...string) result {
	s.t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = s.env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		s.t.Fatal(err)
	}
	return result{code, out.String(), errb.String()}
}

func (s *sandbox) must(want int, args ...string) result {
	s.t.Helper()
	r := s.run(args...)
	if r.code != want {
		s.t.Fatalf("agentsd %s: exit %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, want, r.stdout, r.stderr)
	}
	return r
}

func (s *sandbox) startDaemon() {
	s.t.Helper()
	cmd := exec.Command(bin, "daemon", "--grace", "2s")
	cmd.Env = s.env
	logf, _ := os.Create(filepath.Join(s.home, ".daemon-test.out"))
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	s.daemon = cmd
	for i := 0; i < 100; i++ {
		if s.run("status").code == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(filepath.Join(s.home, ".daemon-test.out"))
	s.t.Fatalf("daemon did not come up:\n%s", b)
}

func (s *sandbox) stopDaemon() {
	if s.daemon == nil || s.daemon.Process == nil {
		return
	}
	s.daemon.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { s.daemon.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		s.daemon.Process.Kill()
	}
	s.daemon = nil
}

func (s *sandbox) killDaemon() {
	s.daemon.Process.Kill()
	s.daemon.Wait()
	s.daemon = nil
}

func (s *sandbox) writeAgent(name, body string) {
	s.t.Helper()
	p := filepath.Join(s.home, ".config", "agentsd", "agents", name+".toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, s)
	}
	return m
}

const sleeper = `name = "sleeper"
runtime = "exec"
[workspace]
cwd = "~/src"
read = ["~/src"]
write = ["~/wiki/notes"]
[runtime_options]
command = ["/bin/sh", "-c", "echo started; sleep 30"]
`

// §15: init → apply → run example, identical XDG paths; plan idempotent.
func TestLifecycle(t *testing.T) {
	s := newSandbox(t)
	s.must(0, "init")
	s.must(0, "validate")
	if r := s.run("doctor"); r.code != 0 {
		t.Fatalf("doctor failed on a fresh init:\n%s%s", r.stdout, r.stderr)
	}
	s.must(6, "plan", "--exit-code")
	s.must(0, "apply", "-y")
	s.must(0, "plan", "--exit-code")
	for _, p := range []string{".config/agentsd/config.toml", ".config/agentsd/agents/example.toml", ".local/state/agentsd/managed.toml", "src", "wiki", "out/example"} {
		if _, err := os.Stat(filepath.Join(s.home, p)); err != nil {
			t.Errorf("missing %s", p)
		}
	}
	for _, p := range []string{".local/state/agentsd", ".local/state/agentsd/run"} {
		fi, _ := os.Stat(filepath.Join(s.home, p))
		if fi == nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s is not 0700", p)
		}
	}

	s.startDaemon()
	r := s.must(0, "run", "example")
	if !strings.Contains(r.stdout, "hello from example") {
		t.Fatalf("run output: %q", r.stdout)
	}
	list := decode(t, s.must(0, "runs", "list", "--json").stdout)
	runs := list["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs: %v", runs)
	}
	run := runs[0].(map[string]any)
	id := run["id"].(string)
	if run["status"] != "succeeded" {
		t.Fatal(run)
	}
	if _, err := os.Stat(filepath.Join(s.home, "out", "example", id, "hello.txt")); err != nil {
		t.Fatal("hello.txt not written to AGENTSD_OUT")
	}
	if fi, err := os.Stat(filepath.Join(s.home, ".local", "state", "agentsd", "runs", id, "events.jsonl")); err != nil || fi.Size() == 0 {
		t.Fatal("events.jsonl missing")
	}
	if !strings.Contains(s.must(0, "logs", id[:10]).stdout, "hello from example") {
		t.Fatal("logs by prefix")
	}
	// plan remains idempotent with the daemon running.
	s.must(0, "plan", "--exit-code")
}

// §15: killing the daemon mid-run marks the run lost on restart.
func TestLostOnRestart(t *testing.T) {
	s := newSandbox(t)
	s.must(0, "init")
	s.writeAgent("sleeper", sleeper)
	s.must(0, "apply", "-y")
	s.startDaemon()
	id := strings.TrimSpace(s.must(0, "run", "-d", "sleeper").stdout)
	time.Sleep(300 * time.Millisecond)
	s.killDaemon()
	show := decode(t, s.must(0, "runs", "show", id, "--json").stdout)
	pid := int(show["run"].(map[string]any)["pgid"].(float64))
	defer syscall.Kill(-pid, syscall.SIGKILL)
	s.startDaemon()
	show = decode(t, s.must(0, "runs", "show", id, "--json").stdout)
	if st := show["run"].(map[string]any)["status"]; st != "lost" {
		t.Fatalf("status %v, want lost", st)
	}
	if !strings.Contains(s.must(0, "logs", "--events", id).stdout, `"run.lost"`) {
		t.Fatal("no run.lost event")
	}
}

// §15: a run that writes outside write ∪ AGENTSD_OUT produces policy.violation.
func TestPolicyViolation(t *testing.T) {
	s := newSandbox(t)
	s.must(0, "init")
	s.writeAgent("rogue", `name = "rogue"
runtime = "exec"
[workspace]
cwd = "~/src"
read = ["~/src"]
write = ["~/wiki/notes"]
[runtime_options]
command = ["/bin/sh", "-c", "echo ok > $AGENTSD_OUT/a; echo ok > $HOME/wiki/notes/b; echo bad > $HOME/src/x"]
`)
	s.must(0, "apply", "-y")
	s.startDaemon()
	r := s.must(0, "run", "rogue")
	if !strings.Contains(r.stderr, "policy.violation") {
		t.Fatalf("no violation surfaced: %s", r.stderr)
	}
	id := decode(t, s.must(0, "runs", "list", "--json").stdout)["runs"].([]any)[0].(map[string]any)["id"].(string)
	ev := s.must(0, "logs", "--events", id).stdout
	if strings.Count(ev, `"policy.violation"`) != 1 || !strings.Contains(ev, "/src/x") {
		t.Fatalf("events: %s", ev)
	}
	st := decode(t, s.must(0, "status", "--json").stdout)
	if st["policy"].(map[string]any)["violations_7d"].(float64) != 1 {
		t.Fatal(st)
	}
	if !strings.Contains(s.run("doctor").stdout, "violation") {
		t.Fatal("doctor does not surface violations")
	}
}

// §15: doctor detects a missing runtime, a bad socket length, a manifest error.
func TestDoctorDetects(t *testing.T) {
	s := newSandbox(t)
	s.must(0, "init")
	s.writeAgent("ghost", "name = \"ghost\"\nruntime = \"exec\"\n[runtime_options]\ncommand = [\"no-such-runtime-xyz\"]\n")
	r := s.must(0, "doctor", "--json")
	if !strings.Contains(r.stdout, "no-such-runtime-xyz") {
		t.Fatalf("missing runtime not reported: %s", r.stdout)
	}
	s.must(0, "apply", "-y")
	s.startDaemon()
	s.must(7, "run", "ghost")
	s.stopDaemon()

	long := filepath.Join(s.home, strings.Repeat("r", 120))
	os.MkdirAll(long, 0o700)
	s.env = append(s.env, "XDG_RUNTIME_DIR="+long)
	r = s.must(0, "doctor", "--json")
	var rep struct {
		Checks []struct{ Name, Status, Detail string }
	}
	json.Unmarshal([]byte(r.stdout), &rep)
	found := false
	for _, c := range rep.Checks {
		if c.Name == "socket" && c.Status == "warn" {
			found = true
		}
	}
	if !found {
		t.Fatalf("long socket path not flagged: %s", r.stdout)
	}
	s.env = s.env[:len(s.env)-1]

	s.writeAgent("bad", "name = \"bad\"\nruntime = \"exec\"\n[runtime_options]\ncommand = [\"true\"]\n[limits]\ntimeout = \"48h\"\n")
	r = s.must(5, "doctor")
	if !strings.Contains(r.stdout, "fail") || !strings.Contains(r.stdout, "limits.timeout") {
		t.Fatalf("manifest error not failed: %s", r.stdout)
	}
	s.must(5, "validate")
}

// §15: every command supports --json; exit codes match §11.
func TestJSONAndExitCodes(t *testing.T) {
	s := newSandbox(t)
	s.must(0, "init", "--json")
	s.writeAgent("sleeper", sleeper)
	jsonCmds := [][]string{
		{"version"}, {"validate"}, {"doctor"}, {"plan"}, {"apply", "-y"}, {"agent", "list"}, {"agent", "show", "example"},
		{"runs", "list"}, {"service", "status"},
	}
	for _, c := range jsonCmds {
		r := s.run(append(c, "--json")...)
		if r.code != 0 {
			t.Fatalf("%v: exit %d: %s", c, r.code, r.stderr)
		}
		decode(t, r.stdout)
	}
	// Daemon down.
	decode(t, s.must(3, "status", "--json").stdout)
	errBody := decode(t, s.must(3, "run", "example", "--json").stdout)
	if errBody["error"].(map[string]any)["code"] != "daemon_unreachable" {
		t.Fatal(errBody)
	}
	s.must(3, "reload")
	s.must(2, "run")
	s.must(2, "runs", "list", "--status", "bogus")
	s.must(2, "no-such-command")
	s.must(2, "plan", "--no-such-flag")
	s.must(4, "agent", "show", "nobody")
	s.must(4, "runs", "show", "ZZZZ")

	s.startDaemon()
	for _, c := range [][]string{{"status"}, {"reload"}} {
		decode(t, s.must(0, append(c, "--json")...).stdout)
	}
	id := decode(t, s.must(0, "run", "-d", "example", "--json").stdout)["id"].(string)
	time.Sleep(300 * time.Millisecond)
	decode(t, s.must(0, "runs", "show", id, "--json").stdout)
	s.must(0, "logs", id, "--json")
	s.must(4, "run", "unregistered-agent")
	s.must(5, "run", "sleeper", "--cwd", "/usr")
	s.must(2, "run", "example", "--timeout", "1h")

	sid := strings.TrimSpace(s.must(0, "run", "-d", "sleeper").stdout)
	time.Sleep(200 * time.Millisecond)
	s.must(8, "run", "sleeper")
	decode(t, s.must(0, "runs", "stop", sid, "--grace", "1s", "--json").stdout)
	if st := decode(t, s.must(0, "runs", "show", sid, "--json").stdout)["run"].(map[string]any)["status"]; st != "stopped" {
		t.Fatal(st)
	}

	s.writeAgent("fails", "name = \"fails\"\nruntime = \"exec\"\n[runtime_options]\ncommand = [\"/bin/sh\", \"-c\", \"exit 3\"]\n")
	s.writeAgent("slow", "name = \"slow\"\nruntime = \"exec\"\n[limits]\ntimeout = \"1s\"\n[runtime_options]\ncommand = [\"/bin/sh\", \"-c\", \"sleep 20\"]\n")
	s.writeAgent("coder", "name = \"coder\"\nruntime = \"claude-code\"\n")
	s.must(0, "apply", "-y")
	s.must(10, "run", "fails")
	s.must(12, "run", "slow")
	r := s.runIn([]byte("prompt from stdin"), "run", "coder", "-")
	if r.code != 0 || !strings.Contains(r.stdout, "claude -p") {
		t.Fatalf("stdin prompt: %d %q %q", r.code, r.stdout, r.stderr)
	}
	r = s.must(0, "run", "coder", "hello world")
	if !strings.Contains(r.stdout, "claude -p hello world") {
		t.Fatal(r.stdout)
	}
	s.must(2, "run", "coder") // non-exec runtimes need a prompt

	var bash bytes.Buffer
	bash.WriteString(s.must(0, "completion", "bash").stdout)
	if !strings.Contains(bash.String(), "agentsd") {
		t.Fatal("completion")
	}
}

// §15: no file written outside XDG dirs, AHS roots (and the service file).
func TestNoWritesOutsideAllowed(t *testing.T) {
	s := newSandbox(t)
	before := snapshot(t, s.home)
	s.must(0, "init")
	s.writeAgent("sleeper", sleeper)
	s.must(0, "apply", "-y")
	s.startDaemon()
	s.must(0, "run", "example")
	id := strings.TrimSpace(s.must(0, "run", "-d", "sleeper").stdout)
	time.Sleep(200 * time.Millisecond)
	s.must(0, "runs", "stop", id, "--grace", "1s")
	s.stopDaemon()
	s.must(0, "doctor")

	allowed := []string{".config", ".local/state", ".local/share", ".cache", "src", "wiki", "out"}
	for p := range snapshot(t, s.home) {
		if before[p] || strings.HasPrefix(p, ".fakebin") || p == ".daemon-test.out" {
			continue
		}
		ok := false
		for _, a := range allowed {
			if p == a || strings.HasPrefix(p, a+"/") || strings.HasPrefix(a, p+"/") || p == ".local" {
				ok = true
			}
		}
		if !ok {
			t.Errorf("unexpected write: ~/%s", p)
		}
	}
}

// §15: uninstall --purge-state leaves config and AHS roots untouched.
func TestUninstallPurge(t *testing.T) {
	s := newSandbox(t)
	s.must(0, "init")
	s.must(0, "apply", "-y")
	s.startDaemon()
	s.must(0, "run", "example")
	s.must(5, "uninstall", "--purge-state") // refuses while a daemon runs
	s.stopDaemon()
	os.WriteFile(filepath.Join(s.home, "wiki", "keep.md"), []byte("x"), 0o644)
	s.must(0, "uninstall", "--purge-state")
	for _, gone := range []string{".local/state/agentsd", ".cache/agentsd"} {
		if _, err := os.Stat(filepath.Join(s.home, gone)); err == nil {
			t.Errorf("%s survived purge", gone)
		}
	}
	for _, kept := range []string{".config/agentsd/config.toml", ".config/agentsd/agents/example.toml", "src", "wiki/keep.md", "out/example"} {
		if _, err := os.Stat(filepath.Join(s.home, kept)); err != nil {
			t.Errorf("%s was removed", kept)
		}
	}
}

func snapshot(t *testing.T, root string) map[string]bool {
	t.Helper()
	m := map[string]bool{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel != "." {
			m[rel] = true
		}
		return nil
	})
	return m
}
