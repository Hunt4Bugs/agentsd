package runstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mk(t *testing.T, s *Store, at time.Time, agent string, st Status) *Run {
	t.Helper()
	r := &Run{ID: NewID(at), Agent: agent, Status: st, CreatedAt: at}
	if err := s.Create(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCreateListResolve(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "runs")}
	now := time.Now()
	a := mk(t, s, now.Add(-2*time.Hour), "a", Succeeded)
	b := mk(t, s, now.Add(-time.Hour), "b", Failed)
	c := mk(t, s, now, "a", Running)

	all, _ := s.List(Filter{})
	if len(all) != 3 || all[0].ID != c.ID || all[2].ID != a.ID {
		t.Fatalf("order: %v", all)
	}
	onlyA, _ := s.List(Filter{Agent: "a"})
	if len(onlyA) != 2 {
		t.Fatal(onlyA)
	}
	failed, _ := s.List(Filter{Status: Failed})
	if len(failed) != 1 || failed[0].ID != b.ID {
		t.Fatal(failed)
	}
	recent, _ := s.List(Filter{Since: now.Add(-90 * time.Minute)})
	if len(recent) != 2 {
		t.Fatal(recent)
	}
	got, err := s.Get(c.ID[:20])
	if err != nil || got.ID != c.ID {
		t.Fatal(err)
	}
	if _, err := s.Get("ZZZZ"); err == nil {
		t.Fatal("expected not found")
	}
	fi, _ := os.Stat(s.Dir(a.ID))
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
}

func TestPrune(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "runs")}
	now := time.Now()
	old := mk(t, s, now.Add(-100*24*time.Hour), "a", Succeeded)
	oldRunning := mk(t, s, now.Add(-99*24*time.Hour), "a", Running)
	for i := range 5 {
		mk(t, s, now.Add(-time.Duration(i)*time.Minute), "a", Succeeded)
	}
	n, _ := s.Prune(3, 90, now, nil)
	ids, _ := s.IDs()
	if n != 3 || len(ids) != 4 {
		t.Fatalf("removed %d, left %d", n, len(ids))
	}
	if _, err := s.Load(old.ID); err == nil {
		t.Fatal("old run survived")
	}
	if _, err := s.Load(oldRunning.ID); err != nil {
		t.Fatal("running run was pruned")
	}
}

func TestReadLogsMerged(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	r := mk(t, s, time.Now(), "a", Succeeded)
	t0 := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	os.WriteFile(filepath.Join(s.Dir(r.ID), StdoutFile), []byte(FormatLogLine(t0, "one")+FormatLogLine(t0.Add(2), "three")), 0o600)
	os.WriteFile(filepath.Join(s.Dir(r.ID), StderrFile), []byte(FormatLogLine(t0.Add(1), "two has  spaces")), 0o600)
	lines, err := s.ReadLogs(r.ID, Stdout, Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || lines[1].Text != "two has  spaces" || lines[1].Stream != Stderr || lines[2].Text != "three" {
		t.Fatalf("%+v", lines)
	}
}
