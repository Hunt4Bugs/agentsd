package policy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScan(t *testing.T) {
	home := t.TempDir()
	src, wiki, out := filepath.Join(home, "src"), filepath.Join(home, "wiki"), filepath.Join(home, "out", "a", "RUN")
	for _, d := range []string{src, filepath.Join(wiki, "research"), out, filepath.Join(src, "deep")} {
		os.MkdirAll(d, 0o755)
	}
	old := filepath.Join(src, "old.txt")
	os.WriteFile(old, []byte("x"), 0o644)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(old, past, past)

	// Written moments before the run in the same second: not the run's.
	os.WriteFile(filepath.Join(src, "just-before.txt"), []byte("x"), 0o644)
	time.Sleep(10 * time.Millisecond)

	start := time.Now()
	os.WriteFile(filepath.Join(src, "deep", "bad.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(wiki, "research", "ok.md"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(out, "ok.txt"), []byte("x"), 0o644)
	end := time.Now()

	res := Scan(ScanInput{
		Roots:   []string{src, wiki, filepath.Join(home, "out"), src},
		Allowed: []string{filepath.Join(wiki, "research"), out},
		Start:   start, End: end,
	})
	if len(res.Violations) != 1 || filepath.Base(res.Violations[0].Path) != "bad.txt" {
		t.Fatalf("%+v", res.Violations)
	}
	if res.Truncated {
		t.Fatal("unexpected truncation")
	}
}

func TestScanTruncates(t *testing.T) {
	dir := t.TempDir()
	for i := range 20 {
		os.WriteFile(filepath.Join(dir, string(rune('a'+i))), nil, 0o644)
	}
	res := Scan(ScanInput{Roots: []string{dir}, Start: time.Now(), End: time.Now(), MaxEntries: 5})
	if !res.Truncated {
		t.Fatal("expected truncation")
	}
}
