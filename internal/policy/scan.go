// Package policy implements advisory-mode policy (spec §8). The post-run scan
// is best-effort: it attributes files by mtime to the run window, so writes by
// other processes in that window can show up, and writes that restore an old
// mtime cannot.
package policy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/paths"
)

// Scan bounds.
const (
	DefaultMaxEntries = 200_000
	DefaultMaxTime    = 10 * time.Second
)

type ScanInput struct {
	// Roots are walked (AHS roots plus the run's cwd).
	Roots []string
	// Allowed are writable for the run (workspace.write plus AGENTSD_OUT);
	// they are not descended into.
	Allowed []string
	// Skip are not descended into either (agentsd's own dirs, other active
	// runs' output dirs).
	Skip       []string
	Start, End time.Time
	MaxEntries int
	MaxTime    time.Duration
}

type Violation struct {
	Path  string    `json:"path"`
	MTime time.Time `json:"mtime"`
}

type Result struct {
	Violations []Violation
	Entries    int
	Truncated  bool
	Elapsed    time.Duration
	Roots      []string
}

var errStop = errors.New("stop")

// Scan walks the roots for regular files and symlinks whose mtime falls in the
// run window and that lie outside Allowed. Directories are not reported: their
// mtime also changes when an allowed child is created.
func Scan(in ScanInput) Result {
	if in.MaxEntries == 0 {
		in.MaxEntries = DefaultMaxEntries
	}
	if in.MaxTime == 0 {
		in.MaxTime = DefaultMaxTime
	}
	began := time.Now()
	deadline := began.Add(in.MaxTime)
	// Filesystems with coarse (1s) mtimes can hide writes made in the run's
	// first second; that is an accepted best-effort gap.
	lo, hi := in.Start, in.End.Add(time.Second)
	res := Result{}
	seen := map[string]bool{}
	prune := make([]string, 0, len(in.Allowed)+len(in.Skip))
	for _, p := range append(append([]string{}, in.Allowed...), in.Skip...) {
		prune = append(prune, paths.Real(p))
	}
	roots := dedupeRoots(in.Roots)
	res.Roots = roots
	for _, root := range roots {
		root = paths.Real(root)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			res.Entries++
			if res.Entries > in.MaxEntries || (res.Entries%1024 == 0 && time.Now().After(deadline)) {
				res.Truncated = true
				return errStop
			}
			if d.IsDir() {
				for _, a := range prune {
					if paths.Within(p, a) {
						return fs.SkipDir
					}
				}
				return nil
			}
			if !d.Type().IsRegular() && d.Type()&fs.ModeSymlink == 0 {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			mt := info.ModTime()
			if mt.Before(lo) || mt.After(hi) || seen[p] {
				return nil
			}
			for _, a := range prune {
				if paths.Within(p, a) {
					return nil
				}
			}
			seen[p] = true
			res.Violations = append(res.Violations, Violation{Path: p, MTime: mt})
			return nil
		})
		if errors.Is(err, errStop) {
			break
		}
	}
	sort.Slice(res.Violations, func(i, j int) bool { return res.Violations[i].Path < res.Violations[j].Path })
	res.Elapsed = time.Since(began)
	return res
}

// dedupeRoots drops roots nested inside other roots.
func dedupeRoots(roots []string) []string {
	var out []string
	sorted := append([]string{}, roots...)
	sort.Strings(sorted)
	for _, r := range sorted {
		if r == "" {
			continue
		}
		nested := false
		for _, o := range out {
			if paths.Within(r, o) {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, r)
		}
	}
	return out
}

// FSNow returns "now" as the filesystem will stamp it, by touching a probe
// file in dir. Linux stamps mtimes from a coarse kernel clock that can lag
// time.Now() by a few milliseconds, so a window opened with time.Now() would
// miss writes made right after it. Falls back to time.Now() on error.
func FSNow(dir string) time.Time {
	now := time.Now()
	p := filepath.Join(dir, ".agentsd-clock")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		return now
	}
	fi, err := os.Stat(p)
	if err != nil || fi.ModTime().After(now) {
		return now
	}
	return fi.ModTime()
}
