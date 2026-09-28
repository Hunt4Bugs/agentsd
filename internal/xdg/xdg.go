// Package xdg resolves XDG base directories identically on macOS and Linux
// (spec §3.1). It never uses ~/Library/Application Support.
package xdg

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

const App = "agentsd"

// Dirs are the resolved base directories.
type Dirs struct {
	Home       string `json:"home"`
	ConfigHome string `json:"config_home"`
	StateHome  string `json:"state_home"`
	DataHome   string `json:"data_home"`
	CacheHome  string `json:"cache_home"`
	// RuntimeDir is $XDG_RUNTIME_DIR when set, else empty.
	RuntimeDir string `json:"runtime_dir,omitempty"`
}

// Resolve reads the process environment.
func Resolve() (Dirs, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, fmt.Errorf("resolve home directory: %w", err)
	}
	return FromEnv(os.Getenv, home), nil
}

// FromEnv resolves using getenv. Relative values are ignored, as the XDG spec
// requires.
func FromEnv(getenv func(string) string, home string) Dirs {
	pick := func(key, def string) string {
		if v := getenv(key); v != "" && filepath.IsAbs(v) {
			return filepath.Clean(v)
		}
		return def
	}
	d := Dirs{
		Home:       home,
		ConfigHome: pick("XDG_CONFIG_HOME", filepath.Join(home, ".config")),
		StateHome:  pick("XDG_STATE_HOME", filepath.Join(home, ".local", "state")),
		DataHome:   pick("XDG_DATA_HOME", filepath.Join(home, ".local", "share")),
		CacheHome:  pick("XDG_CACHE_HOME", filepath.Join(home, ".cache")),
	}
	if v := getenv("XDG_RUNTIME_DIR"); v != "" && filepath.IsAbs(v) {
		d.RuntimeDir = filepath.Clean(v)
	}
	return d
}

func (d Dirs) Config() string { return filepath.Join(d.ConfigHome, App) }
func (d Dirs) State() string  { return filepath.Join(d.StateHome, App) }
func (d Dirs) Data() string   { return filepath.Join(d.DataHome, App) }
func (d Dirs) Cache() string  { return filepath.Join(d.CacheHome, App) }
func (d Dirs) Runs() string   { return filepath.Join(d.State(), "runs") }

// Run is the agentsd runtime directory: $XDG_RUNTIME_DIR/agentsd, or
// $XDG_STATE_HOME/agentsd/run when XDG_RUNTIME_DIR is unset (the macOS case).
func (d Dirs) Run() string {
	if d.RuntimeDir != "" {
		return filepath.Join(d.RuntimeDir, App)
	}
	return filepath.Join(d.State(), "run")
}

func (d Dirs) PIDFile() string { return filepath.Join(d.Run(), App+".pid") }

// MaxSocketPath is the longest usable Unix socket path on this platform
// (sun_path size minus the terminating NUL).
func MaxSocketPath() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

// Socket returns the socket path and whether the short /tmp fallback was used
// because the preferred path is too long.
func (d Dirs) Socket() (path string, fallback bool) {
	p := filepath.Join(d.Run(), App+".sock")
	if len(p) <= MaxSocketPath() {
		return p, false
	}
	return filepath.Join(FallbackSocketDir(), App+".sock"), true
}

// PreferredSocket is the socket path before any length fallback.
func (d Dirs) PreferredSocket() string { return filepath.Join(d.Run(), App+".sock") }

// FallbackSocketDir is /tmp/agentsd-$UID.
func FallbackSocketDir() string { return fmt.Sprintf("/tmp/%s-%d", App, os.Getuid()) }

// EnsurePrivateDir creates dir with mode 0700 if missing, and verifies it is a
// real directory owned by the current user with no group/other access. Mode
// drift on an owned directory is corrected.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return CheckPrivateDir(dir, true)
}

// CheckPrivateDir verifies ownership and mode of dir; with fix set it chmods
// an owned directory back to 0700.
func CheckPrivateDir(dir string, fix bool) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by uid %d, not %d", dir, st.Uid, os.Getuid())
	}
	if fi.Mode().Perm() != 0o700 {
		if !fix {
			return fmt.Errorf("%s has mode %04o, want 0700", dir, fi.Mode().Perm())
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}
