package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
	"github.com/Hunt4Bugs/agentsd/internal/plan"
)

// computePlan loads everything and diffs it; invalid input is exit 5.
func (a *App) computePlan() (plan.Plan, error) {
	cfg, agents, diags := a.env.LoadAll()
	if diags.HasErrors() {
		a.printDiags(diags)
		return plan.Plan{}, exitcode.New(exitcode.CodeInvalid, "config or manifests are invalid; fix the errors above (see `agentsd validate`)")
	}
	a.printDiags(diags)
	l, err := a.env.Ledger()
	if err != nil {
		return plan.Plan{}, exitcode.Wrap(exitcode.CodeInvalid, err, "read managed.toml")
	}
	return plan.Compute(cfg, agents, l), nil
}

func (a *App) planCmd() *cobra.Command {
	var exitCode bool
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show changes `apply` would make",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := a.computePlan()
			if err != nil {
				return err
			}
			a.emit(map[string]any{"actions": nonNilActions(p), "changes": !p.Empty(), "summary": p.Summary()}, func() {
				a.out("%s", p.Render(a.env.Home))
			})
			if exitCode && !p.Empty() {
				return &exitcode.Silent{Code: exitcode.ChangesPending}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&exitCode, "exit-code", false, "exit 6 when there are pending changes")
	return cmd
}

func (a *App) applyCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Reconcile manifests to the machine and daemon",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := a.computePlan()
			if err != nil {
				return err
			}
			applied := false
			if !p.Empty() {
				if !a.JSON && !a.Quiet {
					a.out("%s\n", p.Render(a.env.Home))
				}
				if !yes && !a.confirm("Apply these changes?") {
					return exitcode.New(exitcode.CodeUsage, "apply cancelled")
				}
				l, err := a.env.Ledger()
				if err != nil {
					return exitcode.Wrap(exitcode.CodeInvalid, err, "read managed.toml")
				}
				if err := plan.Apply(p, l, a.env.Dirs.State()); err != nil {
					return exitcode.Wrap(exitcode.CodeInternal, err, "apply")
				}
				applied = true
			}
			reloaded := a.reloadIfRunning(cmd.Context())
			a.emit(map[string]any{"applied": applied, "actions": nonNilActions(p), "summary": p.Summary(), "daemon_reloaded": reloaded}, func() {
				switch {
				case !applied:
					a.out("%s\n", p.Summary())
				default:
					a.out("Applied. %s\n", p.Summary()[len("Plan: "):])
				}
				if reloaded {
					a.info("daemon reloaded\n")
				}
			})
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

func nonNilActions(p plan.Plan) []plan.Action {
	if p.Actions == nil {
		return []plan.Action{}
	}
	return p.Actions
}

// reloadIfRunning asks a running daemon to reload; false when unreachable.
func (a *App) reloadIfRunning(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c := a.client()
	if _, err := c.Ping(ctx); err != nil {
		return false
	}
	if err := c.Reload(ctx); err != nil {
		a.info("warning: daemon reload failed: %v\n", err)
		return false
	}
	return true
}
