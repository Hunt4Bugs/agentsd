package cli

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
	"github.com/Hunt4Bugs/agentsd/internal/service"
	"github.com/Hunt4Bugs/agentsd/internal/xdg"
)

func (a *App) serviceManager() (service.Manager, error) {
	m, err := service.For(a.env.Dirs)
	if err != nil {
		return nil, exitcode.Wrap(exitcode.CodeUsage, err, "")
	}
	return m, nil
}

func (a *App) serviceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "service", Short: "Install and control the launchd/systemd user service"}
	var envFile string
	install := &cobra.Command{
		Use:   "install",
		Short: "Write the service definition and start the daemon",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := a.serviceManager()
			if err != nil {
				return err
			}
			bin, err := service.StableBinary()
			if err != nil {
				return exitcode.Wrap(exitcode.CodeInternal, err, "locate agentsd binary")
			}
			o := service.InstallOptions{Binary: bin}
			if envFile != "" {
				abs, err := filepath.Abs(envFile)
				if err != nil {
					return exitcode.Wrap(exitcode.CodeUsage, err, "--env-file")
				}
				if _, err := os.Stat(abs); err != nil {
					return exitcode.Wrap(exitcode.CodeUsage, err, "--env-file")
				}
				o.EnvFile = abs
			}
			cfgOverride := ""
			if a.ConfigPath != "" || os.Getenv("AGENTSD_CONFIG") != "" {
				cfgOverride = a.env.ConfigPath
			}
			o.Env = service.CaptureEnv(cfgOverride)
			if err := xdg.EnsurePrivateDir(a.env.Dirs.State()); err != nil {
				return exitcode.Wrap(exitcode.CodeInternal, err, "state dir")
			}
			if err := m.Install(o); err != nil {
				return exitcode.Wrap(exitcode.CodeInternal, err, "install service")
			}
			up := a.waitDaemon(cmd.Context(), 10*time.Second)
			st := m.Status()
			res := map[string]any{"service": st, "binary": bin, "daemon_reachable": up}
			linger := ""
			if runtime.GOOS == "linux" {
				if ok, hint := service.LingerEnabled(); !ok {
					linger = hint
					res["linger_hint"] = hint
				}
			}
			a.emit(res, func() {
				a.out("installed %s (%s)\n", st.Path, m.Name())
				if up {
					a.out("daemon running (pid %d)\n", st.PID)
				} else {
					a.info("warning: daemon not reachable yet; check `agentsd doctor` and %s\n", a.env.DaemonLog())
				}
				if linger != "" {
					a.info("note: lingering is off, so the daemon stops when you log out. To keep it running:\n  %s\n", linger)
				}
			})
			return nil
		},
	}
	install.Flags().StringVar(&envFile, "env-file", "", "env file (KEY=VALUE lines) loaded into the daemon environment, for env.pass secrets")

	simple := func(use, short string, fn func(service.Manager) error, wait bool) *cobra.Command {
		return &cobra.Command{
			Use: use, Short: short, Args: args(cobra.NoArgs),
			RunE: func(cmd *cobra.Command, _ []string) error {
				m, err := a.serviceManager()
				if err != nil {
					return err
				}
				if err := fn(m); err != nil {
					return exitcode.Wrap(exitcode.CodeInternal, err, use)
				}
				if wait {
					a.waitDaemon(cmd.Context(), 10*time.Second)
				}
				st := m.Status()
				a.emit(st, func() { a.printServiceStatus(st) })
				return nil
			},
		}
	}
	uninstall := simple("uninstall", "Stop the daemon and remove the service definition", service.Manager.Uninstall, false)
	start := simple("start", "Start the daemon via the service manager", service.Manager.Start, true)
	stop := simple("stop", "Stop the daemon via the service manager", service.Manager.Stop, false)
	restart := simple("restart", "Restart the daemon (e.g. after an upgrade)", service.Manager.Restart, true)
	status := simple("status", "Show service state", func(service.Manager) error { return nil }, false)
	cmd.AddCommand(install, uninstall, start, stop, restart, status)
	return cmd
}

func (a *App) printServiceStatus(st service.Status) {
	state := "not installed"
	switch {
	case st.Running:
		state = a.paint("32", "running")
	case st.Loaded:
		state = "loaded (" + st.Detail + ")"
	case st.Installed:
		state = "installed, not loaded"
	}
	a.out("service   %s  %s\n", state, st.Path)
	if st.PID > 0 {
		a.out("pid       %d\n", st.PID)
	}
}

// waitDaemon polls the socket until the daemon answers or timeout passes.
func (a *App) waitDaemon(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := a.client().Ping(ctx); err == nil {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func (a *App) uninstallCmd() *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the service; with --purge-state also delete state and cache",
		Long: `Removes the service definition. With --purge-state, also deletes
$XDG_STATE_HOME/agentsd (run history, ledger, logs) and $XDG_CACHE_HOME/agentsd.
Config ($XDG_CONFIG_HOME/agentsd) and the AHS roots (~/src, ~/wiki, ~/out) are
never touched. Remove the binary afterwards with your installer.`,
		Args: args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			res := map[string]any{"service_removed": false, "purged": []string{}}
			if m, err := service.For(a.env.Dirs); err == nil {
				if st := m.Status(); st.Installed || st.Loaded {
					if err := m.Uninstall(); err != nil {
						return exitcode.Wrap(exitcode.CodeInternal, err, "uninstall service")
					}
					res["service_removed"] = true
					res["service_path"] = st.Path
				}
			}
			if purge {
				ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
				_, err := a.client().Ping(ctx)
				cancel()
				if err == nil {
					return exitcode.New(exitcode.CodeInvalid, "a daemon is still running on %s; stop it before purging state", a.env.SocketPath)
				}
				var purged []string
				dirs := []string{a.env.Dirs.State(), a.env.Dirs.Cache(), a.env.Dirs.Run()}
				if a.env.SocketFallback {
					// Shared by every HOME for this UID; only ours when we use it.
					dirs = append(dirs, filepath.Dir(a.env.SocketPath))
				}
				for _, d := range dirs {
					if _, err := os.Lstat(d); errors.Is(err, fs.ErrNotExist) {
						continue
					}
					if err := os.RemoveAll(d); err != nil {
						return exitcode.Wrap(exitcode.CodeInternal, err, "remove "+d)
					}
					purged = append(purged, d)
				}
				if purged != nil {
					res["purged"] = purged
				}
			}
			a.emit(res, func() {
				if res["service_removed"] == true {
					a.out("removed service %v\n", res["service_path"])
				} else {
					a.out("no service installed\n")
				}
				for _, p := range res["purged"].([]string) {
					a.out("deleted %s\n", p)
				}
				a.info("kept config %s and the AHS roots. Remove the binary with your installer (brew uninstall agentsd, or rm the file).\n", filepath.Dir(a.env.ConfigPath))
			})
			return nil
		},
	}
	cmd.Flags().BoolVar(&purge, "purge-state", false, "also delete state and cache directories")
	return cmd
}

func (a *App) completionCmd(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:       "completion bash|zsh|fish",
		Short:     "Generate shell completions",
		Args:      args(cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs)),
		ValidArgs: []string{"bash", "zsh", "fish"},
		RunE: func(cmd *cobra.Command, argv []string) error {
			switch argv[0] {
			case "bash":
				return root.GenBashCompletionV2(a.Stdout, true)
			case "zsh":
				return root.GenZshCompletion(a.Stdout)
			default:
				return root.GenFishCompletion(a.Stdout, true)
			}
		},
	}
}
