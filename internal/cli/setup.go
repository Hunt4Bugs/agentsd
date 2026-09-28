package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Hunt4Bugs/agentsd/internal/config"
	"github.com/Hunt4Bugs/agentsd/internal/diag"
	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
	"github.com/Hunt4Bugs/agentsd/internal/manifest"
	"github.com/Hunt4Bugs/agentsd/internal/version"
	"github.com/Hunt4Bugs/agentsd/internal/xdg"
)

const configTemplate = `# agentsd global config. See https://github.com/Hunt4Bugs/agentsd
# Unknown keys are errors, so typos fail loudly.
version = 1

[daemon]
log_level = "info"            # error | warn | info | debug | trace
max_concurrent_runs = 4       # global cap across all agents

[ahs]
roots = ["~/src", "~/wiki", "~/out"]
create_missing_roots = true   # ` + "`apply`" + ` creates them if absent

[policy]
enforcement = "advisory"      # v0.1 accepts only "advisory"

[runs]
retain = 500                  # keep the most recent N runs; older are pruned
retain_days = 90              # and/or prune runs older than this

[runtimes.claude-code]
command = "claude"            # executable name or absolute path
# argv = ["{command}", "-p", "{prompt}", "{args...}"]

[runtimes.codex]
command = "codex"
# argv = ["{command}", "exec", "{prompt}", "{args...}"]
`

const exampleTemplate = `# An exec agent that writes a greeting to its per-run output directory.
# Try it: agentsd apply && agentsd run example
name = "example"
description = "Writes hello.txt to $AGENTSD_OUT"
runtime = "exec"

[limits]
timeout = "1m"
max_concurrent = 1

[runtime_options]
command = ["/bin/sh", "-c", "echo \"hello from $AGENTSD_AGENT (run $AGENTSD_RUN_ID)\" | tee \"$AGENTSD_OUT/hello.txt\""]
`

type initResult struct {
	Path   string `json:"path"`
	Action string `json:"action"` // created | exists | overwritten
}

func (a *App) initCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create config dirs, config.toml, and an example agent",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			e := a.env
			var results []initResult
			dir := func(p string, private bool) error {
				_, statErr := os.Stat(p)
				var err error
				if private {
					err = xdg.EnsurePrivateDir(p)
				} else {
					err = os.MkdirAll(p, 0o755)
				}
				if err != nil {
					return exitcode.Wrap(exitcode.CodeInternal, err, "create "+p)
				}
				action := "exists"
				if errors.Is(statErr, fs.ErrNotExist) {
					action = "created"
				}
				results = append(results, initResult{p, action})
				return nil
			}
			for _, d := range []struct {
				p       string
				private bool
			}{
				{filepath.Dir(e.ConfigPath), false}, {e.AgentsDir(), false},
				{e.Dirs.State(), true}, {e.Dirs.Runs(), true}, {e.Dirs.Run(), true},
				{e.Dirs.Data(), false}, {e.Dirs.Cache(), false},
			} {
				if err := dir(d.p, d.private); err != nil {
					return err
				}
			}
			file := func(p, content string, overwrite bool) error {
				_, statErr := os.Stat(p)
				exists := statErr == nil
				if exists && !overwrite {
					results = append(results, initResult{p, "exists"})
					return nil
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					return exitcode.Wrap(exitcode.CodeInternal, err, "write "+p)
				}
				action := "created"
				if exists {
					action = "overwritten"
				}
				results = append(results, initResult{p, action})
				return nil
			}
			if err := file(e.ConfigPath, configTemplate, false); err != nil {
				return err
			}
			if err := file(filepath.Join(e.AgentsDir(), "example.toml"), exampleTemplate, force); err != nil {
				return err
			}
			a.emit(map[string]any{"results": results}, func() {
				for _, r := range results {
					a.out("%-12s %s\n", r.Action, r.Path)
				}
				a.info("\nNext: agentsd doctor, then agentsd apply\n")
			})
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite agents/example.toml (never config.toml)")
	return cmd
}

func (a *App) validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [FILE...]",
		Short: "Validate config and manifests",
		RunE: func(cmd *cobra.Command, files []string) error {
			e := a.env
			cfg, diags := e.LoadConfig()
			checked := []string{}
			if len(files) == 0 {
				if cfg.Found {
					checked = append(checked, e.ConfigPath)
				}
				if !diags.HasErrors() {
					_, ad := e.LoadAgents(cfg)
					diags = append(diags, ad...)
					entries, _ := os.ReadDir(e.AgentsDir())
					for _, en := range entries {
						if strings.HasSuffix(en.Name(), ".toml") && !en.IsDir() {
							checked = append(checked, filepath.Join(e.AgentsDir(), en.Name()))
						}
					}
				}
			} else {
				cfgDiags := diags
				diags = nil
				for _, f := range files {
					abs, _ := filepath.Abs(f)
					checked = append(checked, abs)
					if abs == e.ConfigPath || filepath.Base(abs) == "config.toml" {
						_, d := config.Load(abs, e.Home)
						if _, err := os.Stat(abs); err != nil {
							d = append(d, diag.Diagnostic{File: abs, Severity: diag.SevError, Message: err.Error()})
						}
						diags = append(diags, d...)
						continue
					}
					if cfgDiags.HasErrors() {
						diags = append(diags, cfgDiags...)
						cfgDiags = nil
						continue
					}
					_, d := manifest.LoadFile(abs, e.ManifestContext(cfg))
					diags = append(diags, d...)
				}
			}
			valid := !diags.HasErrors()
			if diags == nil {
				diags = diag.List{}
			}
			if a.JSON {
				a.writeJSON(map[string]any{"valid": valid, "files": checked, "diagnostics": diags})
			} else {
				a.printDiags(diags)
				if valid {
					a.out("ok: %d file(s) valid\n", len(checked))
				}
			}
			if !valid {
				return &exitcode.Silent{Code: exitcode.Invalid}
			}
			return nil
		},
	}
}

func (a *App) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			v := map[string]string{"version": version.Version, "commit": version.Commit, "date": version.Date,
				"go": runtime.Version(), "platform": runtime.GOOS + "/" + runtime.GOARCH}
			a.emit(v, func() {
				a.out("agentsd %s", version.Version)
				if version.Commit != "" {
					c := version.Commit
					if len(c) > 12 {
						c = c[:12]
					}
					a.out(" (%s)", c)
				}
				a.out(" %s %s\n", runtime.Version(), v["platform"])
			})
			return nil
		},
	}
}
