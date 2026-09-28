// Package runstore owns the on-disk layout of runs (spec §3.2):
// runs/<id>/{run.json,events.jsonl,stdout.log,stderr.log}.
package runstore

import (
	"crypto/rand"
	"time"

	"github.com/oklog/ulid/v2"
)

type Status string

const (
	Pending   Status = "pending"
	Running   Status = "running"
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
	TimedOut  Status = "timed_out"
	Stopped   Status = "stopped"
	Lost      Status = "lost"
	Rejected  Status = "rejected"
)

// Statuses accepted by `runs list --status`.
var Statuses = []Status{Pending, Running, Succeeded, Failed, Stopped, TimedOut, Lost, Rejected}

// Terminal reports whether s is a final state.
func (s Status) Terminal() bool { return s != Pending && s != Running }

// Run is the content of run.json.
type Run struct {
	ID          string            `json:"id"`
	Agent       string            `json:"agent"`
	Runtime     string            `json:"runtime"`
	Status      Status            `json:"status"`
	Reason      string            `json:"reason,omitempty"` // rejection or failure detail
	ReasonCode  string            `json:"reason_code,omitempty"`
	Prompt      string            `json:"prompt,omitempty"`
	PromptStdin bool              `json:"prompt_stdin,omitempty"`
	Argv        []string          `json:"argv,omitempty"`
	Cwd         string            `json:"cwd"`
	OutDir      string            `json:"out_dir"`
	Labels      map[string]string `json:"labels,omitempty"`
	EnvPassed   []string          `json:"env_passed"`
	EnvMissing  []string          `json:"env_missing,omitempty"`
	Timeout     string            `json:"timeout"`
	CreatedAt   time.Time         `json:"created_at"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	EndedAt     *time.Time        `json:"ended_at,omitempty"`
	PID         int               `json:"pid,omitempty"`
	PGID        int               `json:"pgid,omitempty"`
	DaemonPID   int               `json:"daemon_pid,omitempty"`
	ExitCode    *int              `json:"exit_code,omitempty"`
	Signal      string            `json:"signal,omitempty"`
	Violations  int               `json:"violations"`
	PolicyScan  *ScanSummary      `json:"policy_scan,omitempty"`
}

// ScanSummary records how the post-run policy scan went.
type ScanSummary struct {
	BestEffort bool     `json:"best_effort"`
	Entries    int      `json:"entries"`
	Truncated  bool     `json:"truncated"`
	Duration   string   `json:"duration"`
	Roots      []string `json:"roots"`
}

// Duration returns the run's wall time so far (or total, when ended).
func (r *Run) Duration(now time.Time) time.Duration {
	if r.StartedAt == nil {
		return 0
	}
	end := now
	if r.EndedAt != nil {
		end = *r.EndedAt
	}
	return end.Sub(*r.StartedAt)
}

// NewID returns a new ULID string.
func NewID(t time.Time) string {
	return ulid.MustNew(ulid.Timestamp(t), rand.Reader).String()
}

// IDTime extracts the creation time encoded in a ULID.
func IDTime(id string) (time.Time, bool) {
	u, err := ulid.ParseStrict(id)
	if err != nil {
		return time.Time{}, false
	}
	return ulid.Time(u.Time()), true
}
