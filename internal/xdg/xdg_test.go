package xdg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	d := FromEnv(env(nil), "/home/u")
	if d.ConfigHome != "/home/u/.config" || d.StateHome != "/home/u/.local/state" ||
		d.DataHome != "/home/u/.local/share" || d.CacheHome != "/home/u/.cache" {
		t.Fatalf("defaults: %+v", d)
	}
	if d.Run() != "/home/u/.local/state/agentsd/run" {
		t.Fatalf("run dir: %s", d.Run())
	}
}

func TestOverridesAndRelativeIgnored(t *testing.T) {
	d := FromEnv(env(map[string]string{"XDG_CONFIG_HOME": "/c", "XDG_STATE_HOME": "rel", "XDG_RUNTIME_DIR": "/run/user/1"}), "/h")
	if d.ConfigHome != "/c" || d.StateHome != "/h/.local/state" {
		t.Fatalf("%+v", d)
	}
	if d.Run() != "/run/user/1/agentsd" {
		t.Fatal(d.Run())
	}
}

func TestSocketFallback(t *testing.T) {
	d := FromEnv(env(map[string]string{"XDG_RUNTIME_DIR": "/" + strings.Repeat("x", 120)}), "/h")
	p, fb := d.Socket()
	if !fb || !strings.HasPrefix(p, "/tmp/agentsd-") {
		t.Fatalf("got %s %v", p, fb)
	}
	p, fb = FromEnv(env(nil), "/h").Socket()
	if fb || p != "/h/.local/state/agentsd/run/agentsd.sock" {
		t.Fatalf("got %s %v", p, fb)
	}
}

func TestEnsurePrivateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateDir(dir, false); err == nil {
		t.Fatal("expected mode error")
	}
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
}
