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

	// Stream-control events: sent to followers, never persisted.
	EvStreamEnd       = "stream.end"       // the run's stream is complete
	EvStreamTruncated = "stream.truncated" // follower fell behind; resume with after=<seq>
)

// TimeFormat is fixed-width UTC with nanoseconds, used for event and log
// timestamps so that stored order is total. EventFormat (milliseconds) is for
// timestamps carried inside event data.
const (
	TimeFormat  = "2006-01-02T15:04:05.000000000Z"
	EventFormat = "2006-01-02T15:04:05.000Z"
)

type Event struct {
	TS    string `json:"ts"`
	RunID string `json:"run_id"`
	// Seq is the event's position in the run's stream (events and output
	// lines together), starting at 1.
	Seq  int64          `json:"seq,omitempty"`
	Type string         `json:"type"`
	Data map[string]any `json:"data,omitempty"`
}

// NewEvent builds an event. A live run's sink replaces TS with its own
// strictly increasing timestamp.
func NewEvent(runID, typ string, t time.Time, data map[string]any) Event {
	return Event{TS: t.UTC().Format(TimeFormat), RunID: runID, Type: typ, Data: data}
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
// (as run.output events), in time order, numbered by Seq.
func (s *Store) Replay(id string) ([]Event, error) { return s.ReplayLimited(id, nil) }

// ReplayLimited is Replay reading each file only up to limits[file name]
// bytes (all of it when absent).
func (s *Store) ReplayLimited(id string, limits map[string]int64) ([]Event, error) {
	evs, err := s.readEvents(id, limits[EventsFile], limits != nil)
	if err != nil {
		return nil, err
	}
	type item struct {
		t  time.Time
		ev Event
	}
	items := make([]item, 0, len(evs))
	for _, e := range evs {
		items = append(items, item{e.Time(), e})
	}
	for _, st := range []string{Stdout, Stderr} {
		name := st + ".log"
		lines, err := readLogLimited(filepath.Join(s.Dir(id), name), st, limits[name], limits != nil)
		if err != nil {
			return nil, err
		}
		for _, l := range lines {
			t, _ := time.Parse(time.RFC3339Nano, l.TS)
			items = append(items, item{t, OutputEvent(id, t, l.Stream, l.Text)})
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].t.Before(items[j].t) })
	out := make([]Event, len(items))
	for i, it := range items {
		out[i] = it.ev
		out[i].Seq = int64(i + 1)
	}
	return out, nil
}

// OutputEvent builds a run.output event for one line.
func OutputEvent(runID string, t time.Time, stream, line string) Event {
	return Event{TS: t.UTC().Format(TimeFormat), RunID: runID, Type: EvOutput, Data: map[string]any{"stream": stream, "line": line}}
}
