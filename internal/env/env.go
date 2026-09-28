// Package env resolves where everything lives and loads config, manifests,
// the ledger and run state. It is shared by the CLI and the daemon.
package env

import (
	"os"
	"path/filepath"

	"github.com/Hunt4Bugs/agentsd/internal/adapter"
	"github.com/Hunt4Bugs/agentsd/internal/api"
	"github.com/Hunt4Bugs/agentsd/internal/config"
	"github.com/Hunt4Bugs/agentsd/internal/diag"
	"github.com/Hunt4Bugs/agentsd/internal/ledger"
	"github.com/Hunt4Bugs/agentsd/internal/manifest"
	"github.com/Hunt4Bugs/agentsd/internal/runstore"
	"github.com/Hunt4Bugs/agentsd/internal/xdg"
)

type Env struct {
	Home       string
	Dirs       xdg.Dirs
	ConfigPath string
	// SocketPath is the resolved socket; SocketFallback is set when the
	// preferred path was too long.
	SocketPath     string
	SocketFallback bool
}

// Options are the CLI/env overrides.
type Options struct {
	ConfigPath string // --config / AGENTSD_CONFIG
	SocketPath string // --socket / AGENTSD_SOCKET
}

// Resolve builds an Env from the process environment and overrides.
func Resolve(o Options) (*Env, error) {
	dirs, err := xdg.Resolve()
	if err != nil {
		return nil, err
	}
	e := &Env{Home: dirs.Home, Dirs: dirs, ConfigPath: filepath.Join(dirs.Config(), "config.toml")}
	if v := firstNonEmpty(o.ConfigPath, os.Getenv("AGENTSD_CONFIG")); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return nil, err
		}
		e.ConfigPath = abs
	}
	e.SocketPath, e.SocketFallback = dirs.Socket()
	if v := firstNonEmpty(o.SocketPath, os.Getenv("AGENTSD_SOCKET")); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return nil, err
		}
		e.SocketPath, e.SocketFallback = abs, false
	}
	return e, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (e *Env) AgentsDir() string               { return config.AgentsDir(e.ConfigPath) }
func (e *Env) Store() *runstore.Store          { return &runstore.Store{Root: e.Dirs.Runs()} }
func (e *Env) DaemonLog() string               { return filepath.Join(e.Dirs.State(), "daemon.log") }
func (e *Env) Ledger() (*ledger.Ledger, error) { return ledger.Load(e.Dirs.State()) }

func (e *Env) LoadConfig() (*config.Config, diag.List) { return config.Load(e.ConfigPath, e.Home) }

func (e *Env) ManifestContext(cfg *config.Config) manifest.Context {
	return manifest.Context{Home: e.Home, Dirs: e.Dirs, Config: cfg}
}

// LoadAgents loads every manifest in the agents directory.
func (e *Env) LoadAgents(cfg *config.Config) ([]*manifest.Resolved, diag.List) {
	return manifest.LoadDir(e.AgentsDir(), e.ManifestContext(cfg))
}

// LoadAll loads config and manifests, returning all diagnostics.
func (e *Env) LoadAll() (*config.Config, []*manifest.Resolved, diag.List) {
	cfg, diags := e.LoadConfig()
	if diags.HasErrors() {
		return cfg, nil, diags
	}
	agents, ad := e.LoadAgents(cfg)
	return cfg, agents, append(diags, ad...)
}

// AgentInfos merges manifests on disk with the ledger's registered snapshots.
// Unregistered ledger entries (manifest removed) are included.
func AgentInfos(cfg *config.Config, manifests []*manifest.Resolved, l *ledger.Ledger, store *runstore.Store) []api.Agent {
	var out []api.Agent
	seen := map[string]bool{}
	for _, m := range manifests {
		seen[m.Name] = true
		reg := api.Pending
		if old, ok := l.Agents[m.Name]; ok {
			reg = api.Registered
			if len(manifest.Diff(old, m)) > 0 {
				reg = api.Changed
			}
		}
		out = append(out, info(cfg, m, reg, store))
	}
	for _, a := range l.Registered() {
		if !seen[a.Name] {
			out = append(out, info(cfg, a, api.Unregistered, store))
		}
	}
	return out
}

func info(cfg *config.Config, r *manifest.Resolved, reg string, store *runstore.Store) api.Agent {
	a := api.Agent{Resolved: r, Registration: reg}
	if p, err := adapter.Available(r, cfg); err == nil {
		a.Available, a.RuntimePath = true, p
	} else {
		a.Unavailable = err.Error()
	}
	if runs, _ := store.List(runstore.Filter{Agent: r.Name, Limit: 1}); len(runs) > 0 {
		a.LastRun = &api.RunBrief{ID: runs[0].ID, Status: runs[0].Status, CreatedAt: runs[0].CreatedAt}
	}
	return a
}
