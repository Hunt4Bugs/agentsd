package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingFileGivesDefaults(t *testing.T) {
	c, diags := Load(filepath.Join(t.TempDir(), "config.toml"), "/h")
	if len(diags) != 0 || c.Found {
		t.Fatalf("diags=%v found=%v", diags, c.Found)
	}
	if c.Daemon.MaxConcurrentRuns != 4 || c.Runtimes["claude-code"].Command != "claude" {
		t.Fatalf("%+v", c)
	}
	if strings.Join(c.Roots, ",") != "/h/src,/h/wiki,/h/out" {
		t.Fatal(c.Roots)
	}
}

func TestParseValid(t *testing.T) {
	src := `version = 1
[daemon]
max_concurrent_runs = 2
[runtimes.claude-code]
command = "/opt/claude"
argv = ["{command}", "--print", "{prompt}", "{args...}"]
`
	c, diags := Parse("c.toml", []byte(src), "/h")
	if len(diags) != 0 {
		t.Fatal(diags)
	}
	if c.Daemon.MaxConcurrentRuns != 2 || c.Daemon.LogLevel != "info" {
		t.Fatalf("%+v", c.Daemon)
	}
	if c.Runtimes["codex"].Command != "codex" || c.Runtimes["claude-code"].Command != "/opt/claude" {
		t.Fatalf("%+v", c.Runtimes)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct{ src, key string }{
		{"version = 1\n[daemon]\nlog_levl = \"info\"\n", "daemon.log_levl"},
		{"version = 2\n", "version"},
		{"[daemon]\n", "version"},
		{"version = 1\n[policy]\nenforcement = \"enforce\"\n", "policy.enforcement"},
		{"version = 1\n[daemon]\nlog_level = \"loud\"\n", "daemon.log_level"},
		{"version = 1\n[runtimes.hermes]\ncommand = \"h\"\n", "runtimes.hermes"},
		{"version = 1\n[runtimes.codex]\nargv = [\"codex\"]\n", "runtimes.codex.argv"},
		{"version = 1\n[runtimes.codex]\ncommand = \"op://x\"\n", "runtimes.codex.command"},
	}
	for _, tc := range cases {
		_, diags := Parse("c.toml", []byte(tc.src), "/h")
		if !diags.HasErrors() {
			t.Errorf("%q: expected error", tc.src)
			continue
		}
		if diags[0].Key != tc.key {
			t.Errorf("%q: key = %q, want %q (%v)", tc.src, diags[0].Key, tc.key, diags)
		}
		if diags[0].Line == 0 && tc.key != "version" {
			t.Errorf("%q: missing line", tc.src)
		}
	}
}
