package supervisor

import (
	"bytes"
	"sync"
	"time"
)

const (
	partialFlush = time.Second
	maxLine      = 1 << 20
)

// lineWriter splits a process stream into timestamped lines. A trailing
// partial line is flushed after partialFlush of silence (e.g. a prompt).
type lineWriter struct {
	mu     sync.Mutex
	stream string
	sink   *Sink
	buf    []byte
	timer  *time.Timer
}

func newLineWriter(stream string, sink *Sink) *lineWriter {
	return &lineWriter{stream: stream, sink: sink}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.buf = append(w.buf, p...)
			if len(w.buf) >= maxLine {
				w.flushLocked()
			}
			break
		}
		w.buf = append(w.buf, p[:i]...)
		w.flushLocked()
		p = p[i+1:]
	}
	if len(w.buf) > 0 {
		if w.timer == nil {
			w.timer = time.AfterFunc(partialFlush, w.flushPartial)
		} else {
			w.timer.Reset(partialFlush)
		}
	}
	return n, nil
}

func (w *lineWriter) flushPartial() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.flushLocked()
	}
}

func (w *lineWriter) flushLocked() {
	line := bytes.TrimSuffix(w.buf, []byte("\r"))
	w.sink.Output(w.stream, string(line))
	w.buf = w.buf[:0]
}

// Finish flushes any remaining partial line and stops the flush timer.
func (w *lineWriter) Finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
	if len(w.buf) > 0 {
		w.flushLocked()
	}
}
