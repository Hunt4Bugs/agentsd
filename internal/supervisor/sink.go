package supervisor

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/runstore"
)

// SubscriberBuffer is how many events a follower may lag behind before it is
// cut off (it can then resume). A variable so tests can shrink it.
var SubscriberBuffer = 8192

// Sink serializes everything a run writes (events and output lines). Each item
// gets a strictly increasing timestamp and a sequence number, so a replay
// sorted by time reproduces emission order and a follower can resume after
// the last seq it saw.
type Sink struct {
	mu      sync.Mutex
	store   *runstore.Store
	id      string
	files   map[string]*os.File // by file name
	written map[string]int64    // bytes written per file name
	subs    map[*subscriber]struct{}
	seq     int64
	last    time.Time
	closed  bool
}

type subscriber struct {
	ch        chan runstore.Event
	truncated bool
}

// Subscription is a live follower of one run.
type Subscription struct {
	sink *Sink
	sub  *subscriber
}

// C yields live events; it is closed when the stream ends or the follower is
// cut off (see Truncated).
func (s *Subscription) C() <-chan runstore.Event { return s.sub.ch }

// Truncated reports whether the follower was cut off for falling behind.
func (s *Subscription) Truncated() bool {
	s.sink.mu.Lock()
	defer s.sink.mu.Unlock()
	return s.sub.truncated
}

// Cancel unsubscribes.
func (s *Subscription) Cancel() {
	s.sink.mu.Lock()
	defer s.sink.mu.Unlock()
	if _, ok := s.sink.subs[s.sub]; ok {
		delete(s.sink.subs, s.sub)
		close(s.sub.ch)
	}
}

func openSink(store *runstore.Store, id string) (*Sink, error) {
	s := &Sink{store: store, id: id, files: map[string]*os.File{}, written: map[string]int64{}, subs: map[*subscriber]struct{}{}}
	for _, name := range []string{runstore.EventsFile, runstore.StdoutFile, runstore.StderrFile} {
		f, err := os.OpenFile(filepath.Join(store.Dir(id), name), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.files[name] = f
		if fi, err := f.Stat(); err == nil {
			s.written[name] = fi.Size()
		}
	}
	return s, nil
}

// stamp returns a strictly increasing UTC time.
func (s *Sink) stamp() time.Time {
	t := time.Now().UTC()
	if !t.After(s.last) {
		t = s.last.Add(time.Nanosecond)
	}
	s.last = t
	return t
}

func (s *Sink) write(name, data string) {
	n, _ := s.files[name].WriteString(data)
	s.written[name] += int64(n)
}

// Emit persists an event and broadcasts it. Its timestamp is assigned here.
func (s *Sink) Emit(e runstore.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	e.TS = s.stamp().Format(runstore.TimeFormat)
	s.seq++
	e.Seq = s.seq
	s.write(runstore.EventsFile, string(e.JSON())+"\n")
	s.broadcast(e)
}

// Output persists one output line and broadcasts it as run.output.
func (s *Sink) Output(stream, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	t := s.stamp()
	name := runstore.StdoutFile
	if stream == runstore.Stderr {
		name = runstore.StderrFile
	}
	s.write(name, runstore.FormatLogLine(t, line))
	s.seq++
	e := runstore.OutputEvent(s.id, t, stream, line)
	e.Seq = s.seq
	s.broadcast(e)
}

func (s *Sink) broadcast(e runstore.Event) {
	for sub := range s.subs {
		select {
		case sub.ch <- e:
		default:
			// Too slow: cut the follower off rather than stall the run. It
			// can resume from the last seq it received.
			sub.truncated = true
			delete(s.subs, sub)
			close(sub.ch)
		}
	}
}

// Subscribe registers a follower and returns everything persisted before it,
// with no gap or overlap against the live channel. Files are read after the
// lock is released, up to the offsets recorded while holding it, so the
// running process never waits on a replay.
func (s *Sink) Subscribe() ([]runstore.Event, *Subscription) {
	s.mu.Lock()
	limits := map[string]int64{}
	for name, n := range s.written {
		limits[name] = n
	}
	sub := &subscriber{ch: make(chan runstore.Event, SubscriberBuffer)}
	if s.closed {
		close(sub.ch)
	} else {
		s.subs[sub] = struct{}{}
	}
	s.mu.Unlock()
	replay, _ := s.store.ReplayLimited(s.id, limits)
	return replay, &Subscription{sink: s, sub: sub}
}

// Close ends the stream for all followers and closes the files.
func (s *Sink) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for sub := range s.subs {
		close(sub.ch)
	}
	s.subs = nil
	for _, f := range s.files {
		f.Close()
	}
}
