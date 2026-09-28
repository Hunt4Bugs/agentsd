// Package daemon is the long-running supervisor process (`agentsd daemon`)
// serving the /v1 API over a Unix socket.
package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/config"
	"github.com/Hunt4Bugs/agentsd/internal/env"
	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
	"github.com/Hunt4Bugs/agentsd/internal/ledger"
	"github.com/Hunt4Bugs/agentsd/internal/logfile"
	"github.com/Hunt4Bugs/agentsd/internal/runstore"
	"github.com/Hunt4Bugs/agentsd/internal/supervisor"
	"github.com/Hunt4Bugs/agentsd/internal/version"
	"github.com/Hunt4Bugs/agentsd/internal/xdg"
)

type Options struct {
	Env      *env.Env
	Grace    time.Duration
	LogLevel string // overrides config when set
	Stderr   io.Writer
}

type Daemon struct {
	env     *env.Env
	log     *slog.Logger
	level   *slog.LevelVar
	sup     *supervisor.Supervisor
	store   *runstore.Store
	started time.Time

	mu     sync.RWMutex
	cfg    *config.Config
	ledger *ledger.Ledger
}

// Run starts the daemon and blocks until SIGTERM/SIGINT (or ctx is done).
func Run(ctx context.Context, o Options) error {
	e := o.Env
	for _, d := range []string{e.Dirs.State(), e.Dirs.Runs(), e.Dirs.Run()} {
		if err := xdg.EnsurePrivateDir(d); err != nil {
			return exitcode.Wrap(exitcode.CodeInternal, err, "prepare "+d)
		}
	}
	lw, err := logfile.Open(e.DaemonLog(), 10<<20, 3)
	if err != nil {
		return exitcode.Wrap(exitcode.CodeInternal, err, "open daemon.log")
	}
	defer func() { _ = lw.Close() }()
	stderr := o.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	level := new(slog.LevelVar)
	log := slog.New(slog.NewTextHandler(io.MultiWriter(stderr, lw), &slog.HandlerOptions{Level: level}))

	cfg, diags := e.LoadConfig()
	if diags.HasErrors() {
		return exitcode.New(exitcode.CodeInvalid, "config is invalid; not starting:\n%s", diags.Errors().Error())
	}
	setLevel(level, firstNonEmpty(o.LogLevel, os.Getenv("AGENTSD_LOG_LEVEL"), cfg.Daemon.LogLevel))
	l, err := e.Ledger()
	if err != nil {
		return exitcode.Wrap(exitcode.CodeInvalid, err, "read managed.toml")
	}

	ln, err := listen(e, log)
	if err != nil {
		return err
	}
	pidFile := e.Dirs.PIDFile()
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		ln.Close()
		return exitcode.Wrap(exitcode.CodeInternal, err, "write pid file")
	}
	defer func() {
		os.Remove(e.SocketPath)
		if b, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(os.Getpid()) {
			os.Remove(pidFile)
		}
	}()

	d := &Daemon{env: e, log: log, level: level, store: e.Store(), cfg: cfg, ledger: l, started: time.Now().UTC()}
	d.sup = supervisor.New(d.store, e.Dirs, cfg, log)
	if n, err := d.sup.MarkLost(); err != nil {
		log.Error("marking lost runs", "err", err)
	} else if n > 0 {
		log.Warn("marked runs lost from a previous daemon", "count", n)
	}
	if n, _ := d.store.Prune(cfg.Runs.Retain, cfg.Runs.RetainDays, time.Now(), nil); n > 0 {
		log.Info("pruned runs", "count", n)
	}

	srv := &http.Server{Handler: d.routes(), ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(&uidListener{Listener: ln, uid: currentUID(), log: log}) }()
	log.Info("daemon started", "version", version.Version, "pid", os.Getpid(), "socket", e.SocketPath)

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	for {
		select {
		case s := <-sigs:
			if s == syscall.SIGHUP {
				if err := d.reload(); err != nil {
					log.Error("reload failed", "err", err)
				}
				continue
			}
			log.Info("shutting down", "signal", s.String(), "grace", o.Grace)
		case <-ctx.Done():
			log.Info("shutting down", "reason", "context done")
		case err := <-serveErr:
			if !errors.Is(err, http.ErrServerClosed) {
				return exitcode.Wrap(exitcode.CodeInternal, err, "serve")
			}
		}
		break
	}
	d.sup.Shutdown(o.Grace)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	log.Info("daemon stopped")
	return nil
}

// listen prepares the socket directory and binds, refusing to replace a live
// daemon's socket.
func listen(e *env.Env, log *slog.Logger) (net.Listener, error) {
	dir := filepath.Dir(e.SocketPath)
	if err := xdg.EnsurePrivateDir(dir); err != nil {
		return nil, exitcode.Wrap(exitcode.CodeInternal, err, "socket directory")
	}
	if e.SocketFallback {
		log.Warn("socket path too long; using fallback", "preferred", e.Dirs.PreferredSocket(), "socket", e.SocketPath)
	}
	if fi, err := os.Lstat(e.SocketPath); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, exitcode.New(exitcode.CodeInternal, "%s exists and is not a socket", e.SocketPath)
		}
		if c, err := net.DialTimeout("unix", e.SocketPath, time.Second); err == nil {
			c.Close()
			return nil, exitcode.New(exitcode.CodeInternal, "another daemon is already listening on %s", e.SocketPath)
		}
		if err := os.Remove(e.SocketPath); err != nil {
			return nil, exitcode.Wrap(exitcode.CodeInternal, err, "remove stale socket")
		}
	}
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", e.SocketPath)
	syscall.Umask(old)
	if err != nil {
		return nil, exitcode.Wrap(exitcode.CodeInternal, err, "listen")
	}
	if err := os.Chmod(e.SocketPath, 0o600); err != nil {
		ln.Close()
		return nil, exitcode.Wrap(exitcode.CodeInternal, err, "chmod socket")
	}
	return ln, nil
}

func (d *Daemon) reload() error {
	cfg, diags := d.env.LoadConfig()
	if diags.HasErrors() {
		return exitcode.New(exitcode.CodeInvalid, "config is invalid; keeping the previous config:\n%s", diags.Errors().Error())
	}
	l, err := d.env.Ledger()
	if err != nil {
		return exitcode.Wrap(exitcode.CodeInvalid, err, "read managed.toml")
	}
	d.mu.Lock()
	d.cfg, d.ledger = cfg, l
	d.mu.Unlock()
	d.sup.SetConfig(cfg)
	setLevel(d.level, firstNonEmpty(os.Getenv("AGENTSD_LOG_LEVEL"), cfg.Daemon.LogLevel))
	d.sup.Broadcast(runstore.EvReload, map[string]any{"agents": len(l.Agents)})
	d.log.Info("reloaded", "agents", len(l.Agents))
	return nil
}

func (d *Daemon) state() (*config.Config, *ledger.Ledger) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cfg, d.ledger
}

func setLevel(v *slog.LevelVar, name string) {
	switch name {
	case "error":
		v.Set(slog.LevelError)
	case "warn":
		v.Set(slog.LevelWarn)
	case "debug", "trace":
		v.Set(slog.LevelDebug)
	default:
		v.Set(slog.LevelInfo)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
