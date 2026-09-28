package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hunt4Bugs/agentsd/internal/api"
	"github.com/Hunt4Bugs/agentsd/internal/daemon"
	"github.com/Hunt4Bugs/agentsd/internal/env"
	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
)

func (a *App) daemonCmd() *cobra.Command {
	var grace time.Duration
	var level string
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the daemon in the foreground",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if level == "" && a.Verbose > 0 {
				level = "debug"
			}
			return daemon.Run(cmd.Context(), daemon.Options{Env: a.env, Grace: grace, LogLevel: level, Stderr: a.Stderr})
		},
	}
	cmd.Flags().DurationVar(&grace, "grace", 30*time.Second, "how long active runs get after SIGTERM on shutdown")
	cmd.Flags().StringVar(&level, "log-level", "", "override daemon.log_level")
	return cmd
}

func (a *App) reloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Tell the daemon to re-read config and manifests",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res map[string]any
			if err := a.client().Call(cmd.Context(), "POST", "/v1/reload", nil, &res); err != nil {
				return err
			}
			a.emit(res, func() { a.out("reloaded (%v agents registered)\n", res["agents"]) })
			return nil
		},
	}
}

func (a *App) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Daemon and agent summary",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.client().Ping(cmd.Context())
			daemonUp := err == nil
			if !daemonUp {
				st = a.localStatus()
			}
			a.emit(st, func() {
				if daemonUp {
					a.out("daemon    %s  pid %d  up %s  %s\n", a.paint("32", "running"), st.Daemon.PID,
						humanDuration(time.Since(*st.Daemon.StartedAt)), st.Daemon.Version)
				} else {
					a.out("daemon    %s  (socket %s)\n", a.paint("31", "not running"), st.Daemon.Socket)
				}
				a.out("agents    %d defined, %d available\n", st.Agents.Defined, st.Agents.Available)
				a.out("runs      %d running, %d total (7d), %d failed (7d)\n", st.Runs.Running, st.Runs.Total7d, st.Runs.Failed7d)
				viol := fmt.Sprintf("%d violations (7d)", st.Policy.Violations7d)
				if st.Policy.Violations7d > 0 {
					viol = a.paint("33", viol)
				}
				a.out("policy    %s, %s\n", st.Policy.Enforcement, viol)
			})
			if !daemonUp {
				return &exitcode.Silent{Code: exitcode.DaemonUnreachable}
			}
			return nil
		},
	}
}

// localStatus builds a status from disk when the daemon is down.
func (a *App) localStatus() *api.Status {
	st := &api.Status{Daemon: api.DaemonStatus{Socket: a.env.SocketPath}}
	cfg, agents, _ := a.env.LoadAll()
	l, _ := a.env.Ledger()
	if l != nil {
		for _, info := range env.AgentInfos(cfg, agents, l, a.env.Store()) {
			if info.Registration == api.Registered || info.Registration == api.Changed {
				st.Agents.Defined++
				if info.Available {
					st.Agents.Available++
				}
			}
		}
	}
	st.Runs, st.Policy = api.SummarizeRuns(a.env.Store(), time.Now())
	st.Policy.Enforcement = cfg.Policy.Enforcement
	return st
}
