package runstore

import (
	"slices"
	"strconv"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
)

// DefaultListLimit is the default for `runs list --limit`.
const DefaultListLimit = 20

// ParseFilter validates `runs list` filters from their string forms.
func ParseFilter(agent, status, since, limit string) (Filter, error) {
	f := Filter{Agent: agent, Limit: DefaultListLimit}
	if status != "" {
		if !slices.Contains(Statuses, Status(status)) {
			return f, exitcode.New(exitcode.CodeUsage, "unknown status %q", status)
		}
		f.Status = Status(status)
	}
	if since != "" {
		dur, err := time.ParseDuration(since)
		if err != nil || dur <= 0 {
			return f, exitcode.New(exitcode.CodeUsage, "invalid since %q (e.g. 24h)", since)
		}
		f.Since = time.Now().Add(-dur)
	}
	if limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n < 0 {
			return f, exitcode.New(exitcode.CodeUsage, "invalid limit %q", limit)
		}
		f.Limit = n
	}
	return f, nil
}
