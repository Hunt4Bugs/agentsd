package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/runstore"
)

func (a *App) color() bool { return !a.NoColor && isTTY(a.Stdout) }

func (a *App) paint(code, s string) string {
	if !a.color() {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (a *App) statusText(s runstore.Status) string {
	switch s {
	case runstore.Succeeded:
		return a.paint("32", string(s))
	case runstore.Failed, runstore.TimedOut, runstore.Lost, runstore.Rejected:
		return a.paint("31", string(s))
	case runstore.Running, runstore.Pending:
		return a.paint("36", string(s))
	case runstore.Stopped:
		return a.paint("33", string(s))
	}
	return string(s)
}

func (a *App) table() *tabwriter.Writer {
	return tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
}

// humanDuration renders compact durations: 3d4h, 2h5m, 42s, 350ms.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	}
	d = d.Round(time.Second)
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	s := (d - m*time.Minute) / time.Second
	var b strings.Builder
	switch {
	case days > 0:
		fmt.Fprintf(&b, "%dd%dh", days, h)
	case h > 0:
		fmt.Fprintf(&b, "%dh%dm", h, m)
	default:
		fmt.Fprintf(&b, "%dm%ds", m, s)
	}
	return b.String()
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return humanDuration(time.Since(t)) + " ago"
}
