package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hunt4Bugs/agentsd/internal/api"
	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
	"github.com/Hunt4Bugs/agentsd/internal/paths"
	"github.com/Hunt4Bugs/agentsd/internal/runstore"
	"github.com/Hunt4Bugs/agentsd/internal/supervisor"
)

// runExit maps a finished run's status to the exit code (spec §11).
func runExit(r *runstore.Run) int {
	switch r.Status {
	case runstore.Succeeded:
		return exitcode.OK
	case runstore.Failed:
		return exitcode.RunFailed
	case runstore.Stopped:
		return exitcode.RunStopped
	case runstore.TimedOut:
		return exitcode.RunTimedOut
	case runstore.Rejected:
		return exitcode.FromCode(r.ReasonCode)
	}
	return exitcode.Error
}

func (a *App) runCmd() *cobra.Command {
	var detach bool
	var cwd, timeout string
	var labels []string
	cmd := &cobra.Command{
		Use:   "run NAME [PROMPT]",
		Short: "Start a run (PROMPT - reads stdin)",
		Args:  args(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			req := api.StartRunRequest{Agent: argv[0]}
			if len(argv) == 2 {
				req.Prompt = argv[1]
				if req.Prompt == "-" {
					b, err := io.ReadAll(a.Stdin)
					if err != nil {
						return exitcode.Wrap(exitcode.CodeUsage, err, "read prompt from stdin")
					}
					req.Prompt, req.PromptStdin = string(b), true
				}
			}
			if cwd != "" {
				abs, err := filepath.Abs(cwd)
				if err != nil {
					return exitcode.Wrap(exitcode.CodeUsage, err, "--cwd")
				}
				req.Cwd = abs
			}
			if timeout != "" {
				if d, err := time.ParseDuration(timeout); err != nil || d <= 0 {
					return exitcode.New(exitcode.CodeUsage, "invalid --timeout %q", timeout)
				}
				req.Timeout = timeout
			}
			for _, l := range labels {
				k, v, ok := strings.Cut(l, "=")
				if !ok || k == "" {
					return exitcode.New(exitcode.CodeUsage, "invalid --label %q (want K=V)", l)
				}
				if req.Labels == nil {
					req.Labels = map[string]string{}
				}
				req.Labels[k] = v
			}
			c := a.client()
			run, err := c.StartRun(cmd.Context(), req)
			if err != nil {
				return err
			}
			if detach {
				a.emit(run, func() { fmt.Fprintln(a.Stdout, run.ID) })
				return nil
			}
			a.info("run %s started (%s)\n", run.ID, paths.Contract(run.OutDir, a.env.Home))
			return a.follow(cmd.Context(), run.ID, true, followOpts{stdout: true, stderr: true})
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&detach, "detach", "d", false, "print the run ID and return immediately")
	f.StringVar(&cwd, "cwd", "", "override workspace.cwd (must be within read ∪ write)")
	f.StringVar(&timeout, "timeout", "", "override limits.timeout (downward only)")
	f.StringArrayVar(&labels, "label", nil, "attach metadata K=V (repeatable)")
	return cmd
}

type followOpts struct {
	stdout, stderr, events, timestamps bool
}

// follow streams a live run. With interrupt set, Ctrl-C stops the run
// (SIGTERM, SIGKILL after 10s) and a second Ctrl-C kills it immediately.
func (a *App) follow(ctx context.Context, id string, interrupt bool, o followOpts) error {
	c := a.client()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if interrupt {
		sigs := make(chan os.Signal, 2)
		signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigs)
		go func() {
			n := 0
			for range sigs {
				n++
				grace := supervisor.DefaultStopGrace
				if n > 1 {
					grace = 0
					a.info("\nkilling run %s\n", id)
				} else {
					a.info("\nstopping run %s (Ctrl-C again to kill)\n", id)
				}
				sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
				if _, err := c.StopRun(sctx, id, grace); err != nil {
					a.info("stop failed: %v\n", err)
				}
				scancel()
			}
		}()
	}
	err := c.Follow(ctx, id, func(e runstore.Event) error {
		a.printEvent(e, o)
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	// The stream closes after the final state is saved.
	var run *runstore.Run
	for i := 0; i < 50; i++ {
		run, err = c.GetRun(context.Background(), id)
		if err != nil {
			return err
		}
		if run.Status.Terminal() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !interrupt {
		return nil
	}
	if !a.JSON {
		msg := fmt.Sprintf("run %s %s", run.ID, a.statusText(run.Status))
		if run.StartedAt != nil {
			msg += " in " + humanDuration(run.Duration(time.Now()))
		}
		if run.ExitCode != nil {
			msg += fmt.Sprintf(" (exit %d)", *run.ExitCode)
		} else if run.Signal != "" {
			msg += " (" + run.Signal + ")"
		}
		if run.Violations > 0 {
			msg += a.paint("33", fmt.Sprintf(", %d policy violation(s) — see `agentsd runs show %s`", run.Violations, run.ID))
		}
		a.info("%s\n", msg)
	}
	if code := runExit(run); code != 0 {
		return &exitcode.Silent{Code: code}
	}
	return nil
}

func (a *App) printEvent(e runstore.Event, o followOpts) {
	if e.Type == runstore.EvStreamEnd {
		if a.JSON || o.events {
			fmt.Fprintln(a.Stdout, string(e.JSON()))
		}
		return
	}
	if o.events {
		if e.Type != runstore.EvOutput {
			fmt.Fprintln(a.Stdout, string(e.JSON()))
		}
		return
	}
	if a.JSON {
		fmt.Fprintln(a.Stdout, string(e.JSON()))
		return
	}
	if e.Type != runstore.EvOutput {
		if e.Type == runstore.EvViolation && !a.Quiet {
			fmt.Fprintf(a.Stderr, "%s %v\n", a.paint("33", "policy.violation (advisory, best-effort):"), e.Data["path"])
		}
		return
	}
	stream, _ := e.Data["stream"].(string)
	line, _ := e.Data["line"].(string)
	if (stream == runstore.Stdout && !o.stdout) || (stream == runstore.Stderr && !o.stderr) {
		return
	}
	a.printLine(runstore.Line{TS: e.TS, Stream: stream, Text: line}, o.timestamps)
}

func (a *App) printLine(l runstore.Line, timestamps bool) {
	w := a.Stdout
	if l.Stream == runstore.Stderr {
		w = a.Stderr
	}
	if a.JSON {
		b, _ := json.Marshal(l)
		fmt.Fprintln(a.Stdout, string(b))
		return
	}
	if timestamps {
		fmt.Fprintf(w, "%s %s\n", l.TS, l.Text)
		return
	}
	fmt.Fprintln(w, l.Text)
}

func (a *App) runsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "runs", Short: "Inspect and control runs"}
	cmd.AddCommand(a.runsListCmd(), a.runsShowCmd(), a.runsStopCmd())
	return cmd
}

func (a *App) runsTable(runs []*runstore.Run) {
	tw := a.table()
	fmt.Fprintln(tw, "ID\tAGENT\tSTATUS\tCREATED\tDURATION\tEXIT\tVIOLATIONS")
	for _, r := range runs {
		dur, exit := "-", "-"
		if r.StartedAt != nil {
			dur = humanDuration(r.Duration(time.Now()))
		}
		if r.ExitCode != nil {
			exit = fmt.Sprint(*r.ExitCode)
		} else if r.Signal != "" {
			exit = r.Signal
		}
		viol := fmt.Sprint(r.Violations)
		if r.Violations > 0 {
			viol = a.paint("33", viol)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Agent, a.statusText(r.Status), ago(r.CreatedAt), dur, exit, viol)
	}
	tw.Flush()
}

func (a *App) runsListCmd() *cobra.Command {
	var agent, status, since string
	var limit int
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List runs",
		Args:    args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := runstore.ParseFilter(agent, status, since, fmt.Sprint(limit))
			if err != nil {
				return err
			}
			runs, err := a.env.Store().List(f)
			if err != nil {
				return exitcode.Wrap(exitcode.CodeInternal, err, "list runs")
			}
			if runs == nil {
				runs = []*runstore.Run{}
			}
			a.emit(map[string]any{"runs": runs}, func() {
				if len(runs) == 0 {
					a.out("no runs\n")
					return
				}
				a.runsTable(runs)
			})
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&agent, "agent", "", "only this agent")
	fl.StringVar(&status, "status", "", "running|succeeded|failed|stopped|timed_out|lost|rejected")
	fl.StringVar(&since, "since", "", "only runs created within this duration (e.g. 24h)")
	fl.IntVar(&limit, "limit", runstore.DefaultListLimit, "maximum runs to show (0 = all)")
	return cmd
}

func (a *App) runsShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show RUN_ID",
		Short: "Show a run (accepts unique ID prefixes)",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			store := a.env.Store()
			r, err := store.Get(argv[0])
			if err != nil {
				return err
			}
			evs, _ := store.ReadEvents(r.ID)
			violations := []map[string]any{}
			for _, e := range evs {
				if e.Type == runstore.EvViolation {
					violations = append(violations, e.Data)
				}
			}
			a.emit(map[string]any{"run": r, "violations": violations}, func() {
				h := a.env.Home
				tw := a.table()
				row := func(k, v string) { fmt.Fprintf(tw, "%s\t%s\n", k, v) }
				row("id", r.ID)
				row("agent", r.Agent)
				row("runtime", r.Runtime)
				row("status", a.statusText(r.Status))
				if r.Reason != "" {
					row("reason", r.Reason)
				}
				row("created", r.CreatedAt.Local().Format(time.RFC3339))
				if r.StartedAt != nil {
					row("started", r.StartedAt.Local().Format(time.RFC3339))
				}
				if r.EndedAt != nil {
					row("ended", r.EndedAt.Local().Format(time.RFC3339))
				}
				if r.StartedAt != nil {
					row("duration", humanDuration(r.Duration(time.Now())))
				}
				row("timeout", r.Timeout)
				if r.ExitCode != nil {
					row("exit code", fmt.Sprint(*r.ExitCode))
				}
				if r.Signal != "" {
					row("signal", r.Signal)
				}
				if r.PID != 0 {
					row("pid / pgid", fmt.Sprintf("%d / %d", r.PID, r.PGID))
				}
				row("cwd", paths.Contract(r.Cwd, h))
				row("output", paths.Contract(r.OutDir, h))
				if len(r.Argv) > 0 {
					row("argv", strings.Join(r.Argv, " "))
				}
				envs := "-"
				if len(r.EnvPassed) > 0 {
					envs = strings.Join(r.EnvPassed, ", ")
				}
				row("env passed", envs)
				if len(r.EnvMissing) > 0 {
					row("env missing", a.paint("33", strings.Join(r.EnvMissing, ", ")))
				}
				for k, v := range r.Labels {
					row("label "+k, v)
				}
				tw.Flush()
				if r.PolicyScan != nil {
					note := fmt.Sprintf("advisory, best-effort: scanned %d entries in %s", r.PolicyScan.Entries, r.PolicyScan.Duration)
					if r.PolicyScan.Truncated {
						note += ", " + a.paint("33", "truncated")
					}
					a.out("\npolicy violations: %d (%s)\n", len(violations), note)
					for _, v := range violations {
						a.out("  %v  (mtime %v)\n", paths.Contract(fmt.Sprint(v["path"]), h), v["mtime"])
					}
				}
			})
			return nil
		},
	}
}

func (a *App) runsStopCmd() *cobra.Command {
	var grace time.Duration
	cmd := &cobra.Command{
		Use:   "stop RUN_ID",
		Short: "Stop a run (SIGTERM, then SIGKILL after --grace)",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			c := a.client()
			id := argv[0]
			if full, err := a.env.Store().Resolve(id); err == nil {
				id = full
			}
			if _, err := c.StopRun(cmd.Context(), id, grace); err != nil {
				return err
			}
			deadline := time.Now().Add(grace + 15*time.Second)
			var run *runstore.Run
			for time.Now().Before(deadline) {
				r, err := c.GetRun(cmd.Context(), id)
				if err != nil {
					return err
				}
				run = r
				if r.Status.Terminal() {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			a.emit(run, func() { a.out("run %s %s\n", run.ID, a.statusText(run.Status)) })
			return nil
		},
	}
	cmd.Flags().DurationVar(&grace, "grace", supervisor.DefaultStopGrace, "time between SIGTERM and SIGKILL")
	return cmd
}

func (a *App) logsCmd() *cobra.Command {
	var o followOpts
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs RUN_ID",
		Short: "Print or follow a run's output",
		Args:  args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			store := a.env.Store()
			r, err := store.Get(argv[0])
			if err != nil {
				return err
			}
			if !o.stdout && !o.stderr {
				o.stdout, o.stderr = true, true
			}
			if follow && !r.Status.Terminal() {
				err := a.follow(cmd.Context(), r.ID, false, o)
				if exitcode.Of(err) == exitcode.DaemonUnreachable {
					return exitcode.Wrap(exitcode.CodeDaemonUnreachable, err, "run "+r.ID+" is "+string(r.Status)+" and following it needs the daemon")
				}
				return err
			}
			if o.events {
				evs, err := store.ReadEvents(r.ID)
				if err != nil {
					return exitcode.Wrap(exitcode.CodeInternal, err, "read events")
				}
				for _, e := range evs {
					fmt.Fprintln(a.Stdout, string(e.JSON()))
				}
				return nil
			}
			var streams []string
			if o.stdout {
				streams = append(streams, runstore.Stdout)
			}
			if o.stderr {
				streams = append(streams, runstore.Stderr)
			}
			lines, err := store.ReadLogs(r.ID, streams...)
			if err != nil {
				return exitcode.Wrap(exitcode.CodeInternal, err, "read logs")
			}
			for _, l := range lines {
				a.printLine(l, o.timestamps)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&follow, "follow", "f", false, "follow an active run (needs the daemon)")
	f.BoolVar(&o.stdout, "stdout", false, "only stdout")
	f.BoolVar(&o.stderr, "stderr", false, "only stderr")
	f.BoolVar(&o.events, "events", false, "print events.jsonl instead of output")
	f.BoolVar(&o.timestamps, "timestamps", false, "prefix each line with its timestamp")
	return cmd
}
