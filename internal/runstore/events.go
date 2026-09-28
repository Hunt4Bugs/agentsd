package runstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Event types (spec §12.2).
const (
	EvCreated   = "run.created"
	EvStarted   = "run.started"
	EvOutput    = "run.output"
	EvExited    = "run.exited"
	EvStopped   = "run.stopped"
	EvTimedOut  = "run.timed_out"
	EvLost      = "run.lost"
	EvViolation = "policy.violation"
	EvReload    = "daemon.reload"
)

// TimeFormat is fixed-width UTC with nanoseconds, so timestamps sort as
// strings. Events use millisecond precision per the spec example.
const (
	TimeFormat  = "2006-01-02T15:04:05.000000000Z"
	EventFormat = "2006-01-02T15:04:05.000Z"
)

type Event struct {
	TS    string         `json:"ts"`
	RunID string         `json:"run_id"`
	Type  string         `json:"type"`
	Data  map[string]any `json:"data,omitempty"`
}

func NewEvent(runID, typ string, t time.Time, data map[string]any) Event {
	return Event{TS: t.UTC().Format(EventFormat), RunID: runID, Type: typ, Data: data}
}

func (e Event) Time() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, e.TS)
	return t
}

func (e Event) JSON() []byte {
	b, _ := json.Marshal(e)
	return b
}

// AppendEvent appends one event to a run's events.jsonl (used when no live
// sink owns the run, e.g. marking runs lost at startup).
func (s *Store) AppendEvent(e Event) error {
	f, err := os.OpenFile(filepath.Join(s.Dir(e.RunID), EventsFile), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(e.JSON(), '\n'))
	return err
}

// Replay returns the run's persisted events merged with its output lines
// (as run.output events), in time order.
func (s *Store) Replay(id string) ([]Event, error) {
	evs, err := s.ReadEvents(id)
	if err != nil {
		return nil, err
	}
	lines, err := s.ReadLogs(id, Stdout, Stderr)
	if err != nil {
		return nil, err
	}
	type item struct {
		t  time.Time
		ev Event
	}
	items := make([]item, 0, len(evs)+len(lines))
	for _, e := range evs {
		items = append(items, item{e.Time(), e})
	}
	for _, l := range lines {
		t, _ := time.Parse(time.RFC3339Nano, l.TS)
		items = append(items, item{t, OutputEvent(id, t, l.Stream, l.Text)})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].t.Before(items[j].t) })
	out := make([]Event, len(items))
	for i, it := range items {
		out[i] = it.ev
	}
	return out, nil
}

// OutputEvent builds a run.output event for one line.
func OutputEvent(runID string, t time.Time, stream, line string) Event {
	return Event{TS: t.UTC().Format(TimeFormat), RunID: runID, Type: EvOutput, Data: map[string]any{"stream": stream, "line": line}}
}
