package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/config"
	"github.com/Hunt4Bugs/agentsd/internal/xdg"
)

func testCtx(t *testing.T) Context {
	home := t.TempDir()
	cfg := config.Default()
	cfg.Roots = []string{filepath.Join(home, "src"), filepath.Join(home, "wiki"), filepath.Join(home, "out")}
	return Context{Home: home, Dirs: xdg.FromEnv(func(string) string { return "" }, home), Config: cfg}
}

const researcher = `name = "researcher"
description = "Reads repositories"
runtime = "claude-code"

[workspace]
cwd = "~/src"
read  = ["~/src", "~/wiki"]
write = ["~/wiki/research"]

[limits]
timeout = "30m"
max_concurrent = 1

[env]
pass = ["ANTHROPIC_API_KEY"]
set  = { RESEARCH_MODE = "deep" }

[runtime_options]
args = []
`

func TestParseResearcher(t *testing.T) {
	ctx := testCtx(t)
	r, diags := Parse("/x/researcher.toml", []byte(researcher), ctx)
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	if r.Cwd != filepath.Join(ctx.Home, "src") || r.Timeout != Duration(30*time.Minute) || r.EnvSet["RESEARCH_MODE"] != "deep" {
		t.Fatalf("%+v", r)
	}
	if r.OutRoot != filepath.Join(ctx.Home, "out", "researcher") {
		t.Fatal(r.OutRoot)
	}
}

func TestParseExecDefaults(t *testing.T) {
	src := "name = \"lint\"\nruntime = \"exec\"\n[runtime_options]\ncommand = [\"/bin/true\"]\n"
	r, diags := Parse("/x/lint.toml", []byte(src), testCtx(t))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	if r.Cwd != "" || r.MaxConcurrent != 1 || r.Timeout != Duration(DefaultTimeout) {
		t.Fatalf("%+v", r)
	}
}

func TestParseErrors(t *testing.T) {
	base := "runtime = \"exec\"\n[runtime_options]\ncommand = [\"x\"]\n"
	cases := []struct {
		file, src, key, msg string
	}{
		{"Bad.toml", "name = \"Bad\"\n" + base, "name", "must match"},
		{"a.toml", "name = \"b\"\n" + base, "name", "does not match file name"},
		{"a.toml", "name = \"a\"\nruntime = \"hermes\"\n", "runtime", "must be one of"},
		{"a.toml", "name = \"a\"\nruntime = \"exec\"\n", "runtime_options.command", "required"},
		{"a.toml", "name = \"a\"\nruntime = \"codex\"\n[runtime_options]\ncommand = [\"x\"]\n", "runtime_options.command", "only valid"},
		{"a.toml", "name = \"a\"\n" + base + "[limits]\ntimeout = \"25h\"\n", "limits.timeout", "exceeds"},
		{"a.toml", "name = \"a\"\n" + base + "[limits]\ntimeout = \"soon\"\n", "limits.timeout", "not a duration"},
		{"a.toml", "name = \"a\"\n" + base + "[workspace]\nwrite = [\"/etc\"]\n", "workspace.write", "outside the AHS roots"},
		{"a.toml", "name = \"a\"\n" + base + "[workspace]\nwrite = [\"~/src/../../etc\"]\n", "workspace.write", "outside"},
		{"a.toml", "name = \"a\"\n" + base + "[workspace]\ncwd = \"~/wiki\"\nread = [\"~/src\"]\n", "workspace.cwd", "not under"},
		{"a.toml", "name = \"a\"\n" + base + "[env]\nset = { AGENTSD_OUT = \"x\" }\n", "env.set.AGENTSD_OUT", "reserved"},
		{"a.toml", "name = \"a\"\n" + base + "[env]\nset = { TOKEN = \"keychain:foo\" }\n", "env.set.TOKEN", "reserved"},
		{"a.toml", "name = \"a\"\n" + base + "[workspace]\nwrit = []\n", "workspace.writ", "unknown key"},
	}
	for _, tc := range cases {
		_, diags := Parse("/x/"+tc.file, []byte(tc.src), testCtx(t))
		if !diags.HasErrors() {
			t.Errorf("%s: expected error for %q", tc.key, tc.src)
			continue
		}
		found := false
		for _, d := range diags {
			if d.Key == tc.key && strings.Contains(d.Message, tc.msg) && (d.Line > 0 || !strings.Contains(tc.src, tc.key[:strings.IndexByte(tc.key+".", '.')])) {
				found = true
			}
		}
		if !found {
			t.Errorf("want %s ~ %q, got %v", tc.key, tc.msg, diags)
		}
	}
}

func TestDurationString(t *testing.T) {
	for d, want := range map[time.Duration]string{30 * time.Minute: "30m", 2 * time.Hour: "2h", 90 * time.Second: "1m30s", 2*time.Hour + 5*time.Minute: "2h5m"} {
		if got := Duration(d).String(); got != want {
			t.Errorf("%v: got %s want %s", d, got, want)
		}
	}
}

func TestAllowOutsideRoots(t *testing.T) {
	src := "name = \"a\"\nruntime = \"exec\"\n[runtime_options]\ncommand = [\"x\"]\n[workspace]\nallow_outside_roots = true\nwrite = [\"/etc/x\"]\n"
	r, diags := Parse("/x/a.toml", []byte(src), testCtx(t))
	if diags.HasErrors() || r.Write[0] != "/etc/x" {
		t.Fatal(diags)
	}
}

func TestLoadDirAndDiff(t *testing.T) {
	ctx := testCtx(t)
	dir := filepath.Join(ctx.Home, "agents")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "researcher.toml"), []byte(researcher), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.toml"), []byte("name = 1"), 0o644)
	agents, diags := LoadDir(dir, ctx)
	if len(agents) != 1 || !diags.HasErrors() {
		t.Fatalf("agents=%d diags=%v", len(agents), diags)
	}
	changed := *agents[0]
	changed.Timeout = Duration(20 * time.Minute)
	d := Diff(agents[0], &changed)
	if len(d) != 1 || d[0] != "limits.timeout 30m → 20m" {
		t.Fatal(d)
	}
}
