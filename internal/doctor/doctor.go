// Package doctor runs environment checks (spec §10 doctor).
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/adapter"
	"github.com/Hunt4Bugs/agentsd/internal/api"
	"github.com/Hunt4Bugs/agentsd/internal/client"
	"github.com/Hunt4Bugs/agentsd/internal/env"
	"github.com/Hunt4Bugs/agentsd/internal/manifest"
	"github.com/Hunt4Bugs/agentsd/internal/paths"
	"github.com/Hunt4Bugs/agentsd/internal/service"
	"github.com/Hunt4Bugs/agentsd/internal/version"
	"github.com/Hunt4Bugs/agentsd/internal/xdg"
)

type Level string

const (
	OK   Level = "ok"
	Warn Level = "warn"
	Fail Level = "fail"
)

type Check struct {
	Name   string `json:"name"`
	Status Level  `json:"status"`
	Detail string `json:"detail"`
}

type Report struct {
	Checks []Check `json:"checks"`
	OK     bool    `json:"ok"`
}

// Run executes all checks.
func Run(ctx context.Context, e *env.Env) Report {
	var r Report
	add := func(name string, st Level, format string, a ...any) {
		r.Checks = append(r.Checks, Check{Name: name, Status: st, Detail: fmt.Sprintf(format, a...)})
	}
	h := func(p string) string { return paths.Contract(p, e.Home) }

	// XDG and permissions.
	d := e.Dirs
	add("xdg", OK, "config=%s state=%s data=%s cache=%s run=%s", h(d.ConfigHome), h(d.StateHome), h(d.DataHome), h(d.CacheHome), h(d.Run()))
	for _, p := range []string{d.State(), d.Run()} {
		switch err := xdg.CheckPrivateDir(p, false); {
		case err == nil:
			add("permissions", OK, "%s is 0700", h(p))
		case os.IsNotExist(err):
			add("permissions", Warn, "%s does not exist (run `agentsd init`)", h(p))
		default:
			add("permissions", Fail, "%v", err)
		}
	}

	// Socket length.
	if e.SocketFallback {
		add("socket", Warn, "preferred socket path %s is %d bytes (limit %d); using fallback %s",
			d.PreferredSocket(), len(d.PreferredSocket()), xdg.MaxSocketPath(), e.SocketPath)
		if _, err := os.Lstat(xdg.FallbackSocketDir()); err == nil {
			if err := xdg.CheckPrivateDir(xdg.FallbackSocketDir(), false); err != nil {
				add("socket", Fail, "fallback socket directory: %v", err)
			}
		}
	} else {
		add("socket", OK, "%s (%d/%d bytes)", h(e.SocketPath), len(e.SocketPath), xdg.MaxSocketPath())
	}

	// Config and manifests.
	cfg, cdiags := e.LoadConfig()
	switch {
	case cdiags.HasErrors():
		for _, dg := range cdiags.Errors() {
			add("config", Fail, "%s", dg.String())
		}
	case !cfg.Found:
		add("config", Warn, "%s not found; using defaults (run `agentsd init`)", h(e.ConfigPath))
	default:
		add("config", OK, "%s valid", h(e.ConfigPath))
	}

	for _, root := range cfg.Roots {
		if fi, err := os.Stat(root); err == nil && fi.IsDir() {
			add("ahs-root", OK, "%s exists", h(root))
		} else {
			add("ahs-root", Warn, "%s missing (run `agentsd apply`)", h(root))
		}
	}

	var agents []*manifest.Resolved
	if !cdiags.HasErrors() {
		as, ad := e.LoadAgents(cfg)
		agents = as
		for _, dg := range ad {
			lvl := Fail
			if dg.Severity != "error" {
				lvl = Warn
			}
			add("manifest", lvl, "%s", dg.String())
		}
		if len(ad.Errors()) == 0 {
			add("manifest", OK, "%d manifest(s) valid in %s", len(agents), h(e.AgentsDir()))
		}
	}

	// Runtimes referenced by agents. claude/codex are asked for --version.
	type use struct {
		agents []string
		probe  bool
	}
	byCmd := map[string]*use{}
	for _, a := range agents {
		c := adapter.Command(a, cfg)
		if byCmd[c] == nil {
			byCmd[c] = &use{}
		}
		byCmd[c].agents = append(byCmd[c].agents, a.Name)
		byCmd[c].probe = byCmd[c].probe || a.Runtime != "exec"
	}
	cmds := make([]string, 0, len(byCmd))
	for c := range byCmd {
		cmds = append(cmds, c)
	}
	sort.Strings(cmds)
	for _, c := range cmds {
		u := byCmd[c]
		users := strings.Join(u.agents, ", ")
		p, err := exec.LookPath(c)
		if err != nil {
			add("runtime", Warn, "%q not found on PATH; unavailable agents: %s (runs exit 7)", c, users)
			continue
		}
		v := ""
		if u.probe {
			v = " " + versionOf(ctx, p)
		}
		add("runtime", OK, "%s → %s%s (used by %s)", c, h(p), v, users)
	}

	// allow_outside_roots.
	for _, a := range agents {
		if a.AllowOutsideRoots {
			add("workspace", Warn, "agent %s sets workspace.allow_outside_roots = true", a.Name)
		}
	}

	// Registration drift.
	if l, err := e.Ledger(); err != nil {
		add("ledger", Fail, "managed.toml: %v", err)
	} else if !cdiags.HasErrors() {
		var drift []string
		for _, info := range env.AgentInfos(cfg, agents, l, e.Store()) {
			if info.Registration != api.Registered {
				drift = append(drift, info.Name+" ("+info.Registration+")")
			}
		}
		if len(drift) > 0 {
			add("apply", Warn, "manifests differ from the applied state: %s (run `agentsd plan`)", strings.Join(drift, ", "))
		} else {
			add("apply", OK, "manifests match the applied state")
		}
	}

	// Service and daemon.
	svcInstalled := false
	if m, err := service.For(d); err == nil {
		st := m.Status()
		svcInstalled = st.Installed
		if st.Installed {
			add("service", OK, "%s installed (%s)", h(st.Path), st.Detail)
		} else {
			add("service", Warn, "not installed (run `agentsd service install`)")
		}
		if runtime.GOOS == "linux" && st.Installed {
			if ok, hint := service.LingerEnabled(); !ok {
				add("linger", Warn, "lingering is off; the daemon stops at logout (run `%s`)", hint)
			}
		}
	}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	st, err := client.New(e.SocketPath).Ping(pctx)
	cancel()
	switch {
	case err != nil && svcInstalled:
		add("daemon", Warn, "not reachable at %s (service installed; see %s)", h(e.SocketPath), h(e.DaemonLog()))
	case err != nil:
		add("daemon", Warn, "not reachable at %s", h(e.SocketPath))
	case st.Daemon.Version != version.Version:
		add("daemon", Fail, "daemon is %s but this CLI is %s (run `agentsd service restart`)", st.Daemon.Version, version.Version)
	default:
		add("daemon", OK, "running, pid %d, %s", st.Daemon.PID, st.Daemon.Version)
	}

	// Policy violations in the last 7 days.
	_, ps := api.SummarizeRuns(e.Store(), time.Now())
	if ps.Violations7d > 0 {
		runs := ps.RunsWithViol
		more := ""
		if len(runs) > 3 {
			more = fmt.Sprintf(" and %d more", len(runs)-3)
			runs = runs[:3]
		}
		add("policy", Warn, "%d advisory violation(s) in the last 7 days, in runs %s%s (see `agentsd runs show`)", ps.Violations7d, strings.Join(runs, ", "), more)
	} else {
		add("policy", OK, "advisory mode, no violations in the last 7 days")
	}

	r.OK = true
	for _, c := range r.Checks {
		if c.Status == Fail {
			r.OK = false
		}
	}
	return r
}

// versionOf runs `<runtime> --version` and returns its first line.
func versionOf(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "(version unknown)"
	}
	return "(" + strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]) + ")"
}
