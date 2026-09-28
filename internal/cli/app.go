// Package cli implements the agentsd command line (spec §4, §10).
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Hunt4Bugs/agentsd/internal/api"
	"github.com/Hunt4Bugs/agentsd/internal/client"
	"github.com/Hunt4Bugs/agentsd/internal/diag"
	"github.com/Hunt4Bugs/agentsd/internal/env"
	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
)

// App holds global flags and I/O for one invocation.
type App struct {
	JSON       bool
	Quiet      bool
	Verbose    int
	NoColor    bool
	ConfigPath string
	SocketPath string

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	env *env.Env
}

// Main runs the CLI and returns the process exit code.
func Main(args []string) int {
	app := &App{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	return app.Run(context.Background(), args)
}

func (a *App) Run(ctx context.Context, args []string) int {
	root := a.rootCmd()
	root.SetArgs(args)
	root.SetOut(a.Stdout)
	root.SetErr(a.Stderr)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return exitcode.OK
	}
	var s *exitcode.Silent
	if errors.As(err, &s) {
		return s.Code
	}
	if !isCoded(err) && isUsageErr(err) {
		err = exitcode.Wrap(exitcode.CodeUsage, err, "")
	}
	a.printErr(err)
	return exitcode.Of(err)
}

func isCoded(err error) bool {
	var e *exitcode.E
	return errors.As(err, &e)
}

func isUsageErr(err error) bool {
	msg := err.Error()
	for _, p := range []string{"unknown command", "unknown flag", "unknown shorthand", "flag needs", "invalid argument", "accepts ", "requires at least", "requires at most", "bad flag"} {
		if strings.HasPrefix(msg, p) || strings.Contains(msg, " "+p) {
			return true
		}
	}
	return false
}

func (a *App) printErr(err error) {
	if a.JSON {
		body := api.ErrorBody{Error: api.ErrorDetail{Code: exitcode.CodeOf(err), Message: err.Error()}}
		var ae *client.APIError
		if errors.As(err, &ae) {
			body.Error.RunID = ae.RunID
		}
		a.writeJSON(body)
	}
	fmt.Fprintf(a.Stderr, "agentsd: %s\n", err)
}

func (a *App) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "agentsd",
		Short:         "Run and supervise AI agents on a machine you own",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if a.NoColor || os.Getenv("NO_COLOR") != "" {
				a.NoColor = true
			}
			e, err := env.Resolve(env.Options{ConfigPath: a.ConfigPath, SocketPath: a.SocketPath})
			if err != nil {
				return exitcode.Wrap(exitcode.CodeInternal, err, "resolve paths")
			}
			a.env = e
			return nil
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return exitcode.Wrap(exitcode.CodeUsage, err, "")
	})
	pf := root.PersistentFlags()
	pf.BoolVar(&a.JSON, "json", false, "machine-readable output on stdout")
	pf.StringVar(&a.ConfigPath, "config", "", "path to config.toml (env AGENTSD_CONFIG)")
	pf.StringVar(&a.SocketPath, "socket", "", "daemon socket path (env AGENTSD_SOCKET)")
	pf.BoolVarP(&a.Quiet, "quiet", "q", false, "errors only")
	pf.CountVarP(&a.Verbose, "verbose", "v", "more output (repeatable)")
	pf.BoolVar(&a.NoColor, "no-color", false, "disable color")

	root.AddGroup(
		&cobra.Group{ID: "setup", Title: "Setup and diagnostics:"},
		&cobra.Group{ID: "decl", Title: "Declarative:"},
		&cobra.Group{ID: "daemon", Title: "Daemon:"},
		&cobra.Group{ID: "agents", Title: "Agents and runs:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("setup", a.initCmd(), a.doctorCmd(), a.validateCmd(), a.versionCmd())
	add("decl", a.planCmd(), a.applyCmd())
	add("daemon", a.daemonCmd(), a.serviceCmd(), a.statusCmd(), a.reloadCmd())
	add("agents", a.agentCmd(), a.runCmd(), a.runsCmd(), a.logsCmd())
	root.AddCommand(a.completionCmd(root), a.uninstallCmd())
	root.SetHelpCommandGroupID("")
	root.SetCompletionCommandGroupID("")
	return root
}

// args wraps a cobra positional-arg validator so failures exit 2.
func args(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, a []string) error {
		if err := v(cmd, a); err != nil {
			return exitcode.Wrap(exitcode.CodeUsage, err, "")
		}
		return nil
	}
}

func (a *App) client() *client.Client { return client.New(a.env.SocketPath) }

// out prints human output to stdout unless --json or --quiet.
func (a *App) out(format string, args ...any) {
	if a.JSON || a.Quiet {
		return
	}
	fmt.Fprintf(a.Stdout, format, args...)
}

// info prints a human message to stderr unless --quiet.
func (a *App) info(format string, args ...any) {
	if a.Quiet {
		return
	}
	fmt.Fprintf(a.Stderr, format, args...)
}

// emit writes v as JSON when --json is set; otherwise calls human.
func (a *App) emit(v any, human func()) {
	if a.JSON {
		a.writeJSON(v)
		return
	}
	if !a.Quiet {
		human()
	}
}

func (a *App) writeJSON(v any) {
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// printDiags prints diagnostics to stderr (always, unless quiet and no errors).
func (a *App) printDiags(diags diag.List) {
	for _, d := range diags {
		if a.Quiet && d.Severity != diag.SevError {
			continue
		}
		fmt.Fprintln(a.Stderr, d.String())
	}
}

// confirm asks y/N on a TTY. Non-interactive stdin counts as yes (spec: apply
// prompts only when stdin is a TTY).
func (a *App) confirm(prompt string) bool {
	if !isTTY(a.Stdin) {
		return true
	}
	fmt.Fprintf(a.Stderr, "%s [y/N] ", prompt)
	line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}

func isTTY(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
