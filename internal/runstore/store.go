package runstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
)

const (
	RunFile    = "run.json"
	EventsFile = "events.jsonl"
	StdoutFile = "stdout.log"
	StderrFile = "stderr.log"
)

// Store is rooted at $XDG_STATE_HOME/agentsd/runs.
type Store struct{ Root string }

func (s *Store) Dir(id string) string { return filepath.Join(s.Root, id) }

// Create makes the run directory and writes the initial run.json.
func (s *Store) Create(r *Run) error {
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(s.Dir(r.ID), 0o700); err != nil {
		return err
	}
	return s.Save(r)
}

// Save atomically replaces run.json.
func (s *Store) Save(r *Run) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(s.Dir(r.ID), RunFile), append(b, '\n'), 0o600)
}

// Load reads one run by exact ID.
func (s *Store) Load(id string) (*Run, error) {
	b, err := os.ReadFile(filepath.Join(s.Dir(id), RunFile))
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", id, err)
	}
	return &r, nil
}

// IDs lists run IDs, newest first.
func (s *Store) IDs() ([]string, error) {
	entries, err := os.ReadDir(s.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && len(e.Name()) == 26 {
			ids = append(ids, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

// Resolve expands a unique ID prefix (case-insensitive).
func (s *Store) Resolve(prefix string) (string, error) {
	prefix = strings.ToUpper(strings.TrimSpace(prefix))
	if prefix == "" {
		return "", exitcode.New(exitcode.CodeUsage, "run ID is empty")
	}
	ids, err := s.IDs()
	if err != nil {
		return "", err
	}
	var match []string
	for _, id := range ids {
		if id == prefix {
			return id, nil
		}
		if strings.HasPrefix(id, prefix) {
			match = append(match, id)
		}
	}
	switch len(match) {
	case 0:
		return "", exitcode.New(exitcode.CodeNotFound, "no run matches %q", prefix)
	case 1:
		return match[0], nil
	default:
		return "", exitcode.New(exitcode.CodeUsage, "run ID prefix %q is ambiguous (%d matches)", prefix, len(match))
	}
}

// Get resolves a prefix and loads the run.
func (s *Store) Get(prefix string) (*Run, error) {
	id, err := s.Resolve(prefix)
	if err != nil {
		return nil, err
	}
	return s.Load(id)
}

// Filter selects runs for List.
type Filter struct {
	Agent  string
	Status Status
	Since  time.Time
	Limit  int
}

// List returns matching runs, newest first. Unreadable runs are skipped.
func (s *Store) List(f Filter) ([]*Run, error) {
	ids, err := s.IDs()
	if err != nil {
		return nil, err
	}
	var out []*Run
	for _, id := range ids {
		if !f.Since.IsZero() {
			if t, ok := IDTime(id); ok && t.Before(f.Since) {
				break // IDs are time-ordered
			}
		}
		r, err := s.Load(id)
		if err != nil {
			continue
		}
		if f.Agent != "" && r.Agent != f.Agent {
			continue
		}
		if f.Status != "" && r.Status != f.Status {
			continue
		}
		out = append(out, r)
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, nil
}

// LastRuns returns the newest run of each named agent in one newest-first
// pass, stopping as soon as every agent has been seen.
func (s *Store) LastRuns(agents []string) map[string]*Run {
	out := map[string]*Run{}
	want := map[string]bool{}
	for _, a := range agents {
		want[a] = true
	}
	ids, _ := s.IDs()
	for _, id := range ids {
		if len(out) == len(want) {
			break
		}
		r, err := s.Load(id)
		if err != nil || !want[r.Agent] || out[r.Agent] != nil {
			continue
		}
		out[r.Agent] = r
	}
	return out
}

// Prune deletes terminal runs beyond the newest retain, and those older than
// retainDays. Zero disables that limit. keep lists IDs that must survive.
func (s *Store) Prune(retain, retainDays int, now time.Time, keep []string) (int, error) {
	ids, err := s.IDs()
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-time.Duration(retainDays) * 24 * time.Hour)
	removed := 0
	for i, id := range ids {
		old := false
		if retainDays > 0 {
			if t, ok := IDTime(id); ok && t.Before(cutoff) {
				old = true
			}
		}
		if (retain <= 0 || i < retain) && !old {
			continue
		}
		if slices.Contains(keep, id) {
			continue
		}
		if r, err := s.Load(id); err == nil && !r.Status.Terminal() {
			continue
		}
		if err := os.RemoveAll(s.Dir(id)); err == nil {
			removed++
		}
	}
	return removed, nil
}

// ReadEvents reads events.jsonl.
func (s *Store) ReadEvents(id string) ([]Event, error) { return s.readEvents(id, 0, false) }

func (s *Store) readEvents(id string, limit int64, limited bool) ([]Event, error) {
	f, err := os.Open(filepath.Join(s.Dir(id), EventsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if limited {
		r = io.LimitReader(f, limit)
	}
	var out []Event
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// WriteFileAtomic writes via a temp file and rename.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
