// Package service installs and controls the per-user service definition: a
// launchd agent on macOS or a systemd user unit on Linux (spec §3.3). These
// are the only files agentsd writes outside XDG dirs and AHS roots.
package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/xdg"
)

const (
	Label    = "dev.agentsd.daemon"
	UnitName = "agentsd.service"
	// StopTimeout gives the daemon's 30s shutdown grace some margin.
	StopTimeout = 45 * time.Second
)

type InstallOptions struct {
	Binary  string
	EnvFile string            // optional secrets env file
	Env     map[string]string // PATH, XDG_*, AGENTSD_CONFIG captured at install time
}

type Status struct {
	Manager   string `json:"manager"`
	Path      string `json:"path"`
	Installed bool   `json:"installed"`
	Loaded    bool   `json:"loaded"`
	Running   bool   `json:"running"`
	PID       int    `json:"pid,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

type Manager interface {
	Name() string
	Path() string
	Render(InstallOptions) string
	Install(InstallOptions) error
	Uninstall() error
	Start() error
	Stop() error
	Restart() error
	Status() Status
}

// Runner executes a command and returns combined output.
type Runner func(name string, args ...string) (string, error)

func execRunner(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil && s != "" {
		return s, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, s)
	}
	return s, err
}

// For returns the manager for this OS.
func For(dirs xdg.Dirs) (Manager, error) {
	switch runtime.GOOS {
	case "darwin":
		return &Launchd{Home: dirs.Home, LogDir: dirs.State(), Run: execRunner, UID: os.Getuid()}, nil
	case "linux":
		return &Systemd{Dir: filepath.Join(dirs.ConfigHome, "systemd", "user"), Run: execRunner}, nil
	}
	return nil, fmt.Errorf("service management is not supported on %s", runtime.GOOS)
}

// CaptureEnv collects the environment the daemon should start with.
func CaptureEnv(configOverride string) map[string]string {
	env := map[string]string{}
	for _, k := range []string{"PATH", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "AGENTSD_LOG_LEVEL", "LANG"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	if configOverride != "" {
		env["AGENTSD_CONFIG"] = configOverride
	}
	return env
}

// StableBinary returns the path to install in the service file. A package
// manager symlink on PATH (e.g. Homebrew's) is preferred over the versioned
// target so upgrades don't break the service.
func StableBinary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(self)
	if err != nil {
		real = self
	}
	if onPath, err := exec.LookPath("agentsd"); err == nil {
		if abs, err := filepath.Abs(onPath); err == nil {
			if r, err := filepath.EvalSymlinks(abs); err == nil && r == real {
				return abs, nil
			}
		}
	}
	return self, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func writeFile(path, content string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), perm)
}
