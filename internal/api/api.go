// Package api defines the /v1 wire types shared by the daemon and the CLI
// (spec §12), plus summaries computed from the state directory.
package api

import (
	"net/http"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
	"github.com/Hunt4Bugs/agentsd/internal/manifest"
	"github.com/Hunt4Bugs/agentsd/internal/runstore"
)

type StartRunRequest struct {
	Agent       string            `json:"agent"`
	Prompt      string            `json:"prompt,omitempty"`
	PromptStdin bool              `json:"prompt_stdin,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Timeout     string            `json:"timeout,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

type StopRunRequest struct {
	Grace string `json:"grace,omitempty"`
}

type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	RunID   string `json:"run_id,omitempty"`
}

// HTTPStatus maps an API error code to an HTTP status.
func HTTPStatus(code string) int {
	switch code {
	case exitcode.CodeNotFound:
		return http.StatusNotFound
	case exitcode.CodeUsage:
		return http.StatusBadRequest
	case exitcode.CodeInvalid, exitcode.CodePolicy:
		return http.StatusUnprocessableEntity
	case exitcode.CodeConcurrency:
		return http.StatusTooManyRequests
	case exitcode.CodeRuntimeUnavailable:
		return http.StatusFailedDependency
	case exitcode.CodeDaemonUnreachable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// Registration states for agent list/show.
const (
	Registered   = "registered"   // manifest matches the applied snapshot
	Pending      = "pending"      // manifest exists but was never applied
	Changed      = "changed"      // manifest differs from the applied snapshot
	Unregistered = "unregistered" // applied, but the manifest is gone
)

type Agent struct {
	*manifest.Resolved
	Available    bool      `json:"available"`
	RuntimePath  string    `json:"runtime_path,omitempty"`
	Unavailable  string    `json:"unavailable_reason,omitempty"`
	Registration string    `json:"registration"`
	LastRun      *RunBrief `json:"last_run,omitempty"`
}

type RunBrief struct {
	ID        string          `json:"id"`
	Status    runstore.Status `json:"status"`
	CreatedAt time.Time       `json:"created_at"`
}

type Status struct {
	Daemon DaemonStatus  `json:"daemon"`
	Agents AgentSummary  `json:"agents"`
	Runs   RunSummary    `json:"runs"`
	Policy PolicySummary `json:"policy"`
}

type DaemonStatus struct {
	Running   bool       `json:"running"`
	PID       int        `json:"pid,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	Uptime    string     `json:"uptime,omitempty"`
	Version   string     `json:"version,omitempty"`
	Socket    string     `json:"socket"`
}

type AgentSummary struct {
	Defined   int `json:"defined"`
	Available int `json:"available"`
}

type RunSummary struct {
	Running  int `json:"running"`
	Total7d  int `json:"total_7d"`
	Failed7d int `json:"failed_7d"`
}

type PolicySummary struct {
	Enforcement  string   `json:"enforcement"`
	Violations7d int      `json:"violations_7d"`
	RunsWithViol []string `json:"runs_with_violations_7d"`
}

// SummarizeRuns counts runs and violations over the last 7 days from disk.
func SummarizeRuns(store *runstore.Store, now time.Time) (RunSummary, PolicySummary) {
	var rs RunSummary
	ps := PolicySummary{RunsWithViol: []string{}}
	runs, _ := store.List(runstore.Filter{Since: now.Add(-7 * 24 * time.Hour)})
	for _, r := range runs {
		rs.Total7d++
		switch r.Status {
		case runstore.Running, runstore.Pending:
			rs.Running++
		case runstore.Failed, runstore.TimedOut, runstore.Lost:
			rs.Failed7d++
		}
		if r.Violations > 0 {
			ps.Violations7d += r.Violations
			ps.RunsWithViol = append(ps.RunsWithViol, r.ID)
		}
	}
	return rs, ps
}
