package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Hunt4Bugs/agentsd/internal/api"
	"github.com/Hunt4Bugs/agentsd/internal/env"
	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
	"github.com/Hunt4Bugs/agentsd/internal/paths"
	"github.com/Hunt4Bugs/agentsd/internal/runstore"
)

func (a *App) agentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Inspect agents"}
	cmd.AddCommand(a.agentListCmd(), a.agentShowCmd())
	return cmd
}

// agentInfos reads manifests, the ledger and run history from disk.
func (a *App) agentInfos() ([]api.Agent, error) {
	cfg, diags := a.env.LoadConfig()
	if diags.HasErrors() {
		a.printDiags(diags)
		return nil, exitcode.New(exitcode.CodeInvalid, "config.toml is invalid")
	}
	agents, diags := a.env.LoadAgents(cfg)
	if !a.JSON {
		a.printDiags(diags)
	}
	l, err := a.env.Ledger()
	if err != nil {
		return nil, exitcode.Wrap(exitcode.CodeInvalid, err, "read managed.toml")
	}
	infos := env.AgentInfos(cfg, agents, l, a.env.Store())
	if infos == nil {
		infos = []api.Agent{}
	}
	return infos, nil
}

func (a *App) agentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List agents",
		Args:    args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			infos, err := a.agentInfos()
			if err != nil {
				return err
			}
			a.emit(map[string]any{"agents": infos}, func() {
				if len(infos) == 0 {
					a.out("no agents defined in %s\n", paths.Contract(a.env.AgentsDir(), a.env.Home))
					return
				}
				tw := a.table()
				fmt.Fprintln(tw, "NAME\tRUNTIME\tAVAILABLE\tREGISTRATION\tLAST RUN")
				for _, i := range infos {
					avail := a.paint("32", "yes")
					if !i.Available {
						avail = a.paint("31", "no")
					}
					last := "-"
					if i.LastRun != nil {
						last = fmt.Sprintf("%s %s", a.statusText(i.LastRun.Status), ago(i.LastRun.CreatedAt))
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", i.Name, i.Runtime, avail, i.Registration, last)
				}
				tw.Flush()
			})
			return nil
		},
	}
}

func (a *App) agentShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show NAME",
		Short: "Show an agent's resolved manifest and recent runs",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			infos, err := a.agentInfos()
			if err != nil {
				return err
			}
			var info *api.Agent
			for i := range infos {
				if infos[i].Name == argv[0] {
					info = &infos[i]
				}
			}
			if info == nil {
				return exitcode.New(exitcode.CodeNotFound, "no agent %q", argv[0])
			}
			runs, _ := a.env.Store().List(runstore.Filter{Agent: info.Name, Limit: 5})
			if runs == nil {
				runs = []*runstore.Run{}
			}
			a.emit(map[string]any{"agent": info, "recent_runs": runs}, func() {
				h := a.env.Home
				c := func(ps []string) string {
					out := make([]string, len(ps))
					for i, p := range ps {
						out[i] = paths.Contract(p, h)
					}
					if len(out) == 0 {
						return "-"
					}
					return strings.Join(out, ", ")
				}
				cwd := paths.Contract(info.Cwd, h)
				if info.Cwd == "" {
					cwd = "(run output directory)"
				}
				tw := a.table()
				row := func(k, v string) { fmt.Fprintf(tw, "%s\t%s\n", k, v) }
				row("name", info.Name)
				if info.Description != "" {
					row("description", info.Description)
				}
				row("runtime", info.Runtime)
				if info.Available {
					row("available", "yes ("+info.RuntimePath+")")
				} else {
					row("available", a.paint("31", "no")+" ("+info.Unavailable+")")
				}
				row("registration", info.Registration)
				row("file", paths.Contract(info.File, h))
				row("workspace.cwd", cwd)
				row("workspace.read", c(info.Read))
				row("workspace.write", c(info.Write))
				row("output", paths.Contract(info.OutRoot, h)+"/<run-id>/ (always writable)")
				if info.AllowOutsideRoots {
					row("allow_outside_roots", a.paint("33", "true"))
				}
				row("limits.timeout", info.Timeout.String())
				row("limits.max_concurrent", fmt.Sprint(info.MaxConcurrent))
				row("env.pass", c(info.EnvPass))
				setKeys := make([]string, 0, len(info.EnvSet))
				for k := range info.EnvSet {
					setKeys = append(setKeys, k)
				}
				row("env.set", c(setKeys))
				if len(info.Command) > 0 {
					row("runtime_options.command", strings.Join(info.Command, " "))
				}
				row("runtime_options.args", c(info.Args))
				tw.Flush()
				a.out("\nrecent runs:\n")
				if len(runs) == 0 {
					a.out("  none\n")
					return
				}
				a.runsTable(runs)
			})
			return nil
		},
	}
}
