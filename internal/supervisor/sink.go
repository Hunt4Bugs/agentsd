package supervisor

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/runstore"
)

// subBuffer is how many events a follower may lag behind before it is cut off.
const subBuffer = 8192

// Sink serializes everything a run writes (events and output lines) so that a
// follower can atomically replay what is on disk and then receive live events
// without gaps or duplicates.
type Sink struct {
	mu     sync.Mutex
	store  *runstore.Store
	id     string
	events *os.File
	logs   map[string]*os.File
	subs   map[chan runstore.Event]struct{}
	closed bool
}

func openSink(store *runstore.Store, id string) (*Sink, error) {
	dir := store.Dir(id)
	open := func(name string) (*os.File, error) {
		return os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	}
	ev, err := open(runstore.EventsFile)
	if err != nil {
		return nil, err
	}
	s := &Sink{store: store, id: id, events: ev, logs: map[string]*os.File{}, subs: map[chan runstore.Event]struct{}{}}
	for stream, name := range map[string]string{runstore.Stdout: runstore.StdoutFile, runstore.Stderr: runstore.StderrFile} {
		f, err := open(name)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.logs[stream] = f
	}
	return s, nil
}

// Emit persists an event and broadcasts it.
func (s *Sink) Emit(e runstore.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	_, _ = s.events.Write(append(e.JSON(), '\n'))
	s.broadcast(e)
}

// Output persists one output line and broadcasts it as run.output.
func (s *Sink) Output(stream string, t time.Time, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	_, _ = s.logs[stream].WriteString(runstore.FormatLogLine(t, line))
	s.broadcast(runstore.OutputEvent(s.id, t, stream, line))
}

func (s *Sink) broadcast(e runstore.Event) {
	for ch := range s.subs {
		select {
		case ch <- e:
		default:
			// Too slow: cut the follower off rather than stall the run.
			delete(s.subs, ch)
			close(ch)
		}
	}
}

// Subscribe returns everything persisted so far plus a channel of subsequent
// events. The channel is closed when the run's stream ends.
func (s *Sink) Subscribe() ([]runstore.Event, <-chan runstore.Event, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	replay, _ := s.store.Replay(s.id)
	ch := make(chan runstore.Event, subBuffer)
	if s.closed {
		close(ch)
		return replay, ch, func() {}
	}
	s.subs[ch] = struct{}{}
	return replay, ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
	}
}

// Close ends the stream for all followers and closes the files.
func (s *Sink) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for ch := range s.subs {
		close(ch)
	}
	s.subs = nil
	if s.events != nil {
		s.events.Close()
	}
	for _, f := range s.logs {
		f.Close()
	}
}
