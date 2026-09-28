package ledger

import (
	"testing"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/manifest"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	l, _ := Load(dir)
	l.Dirs = append(l.Dirs, Dir{Path: "/h/wiki/research", Reason: "researcher: workspace.write", CreatedAt: time.Now().UTC().Truncate(time.Second)})
	l.Agents["researcher"] = &manifest.Resolved{Name: "researcher", Runtime: "claude-code", Timeout: manifest.Duration(30 * time.Minute),
		MaxConcurrent: 1, Read: []string{"/h/src"}, Write: []string{}, EnvPass: []string{}, EnvSet: map[string]string{"A": "b"}, Args: []string{}}
	if err := l.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasDir("/h/wiki/research") {
		t.Fatal("dir lost")
	}
	a := got.Agents["researcher"]
	if a == nil || a.Timeout != manifest.Duration(30*time.Minute) || a.EnvSet["A"] != "b" {
		t.Fatalf("%+v", a)
	}
	if d := manifest.Diff(l.Agents["researcher"], a); len(d) != 0 {
		t.Fatalf("round trip changed agent: %v", d)
	}
}
