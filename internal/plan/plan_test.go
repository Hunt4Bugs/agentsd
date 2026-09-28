package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/config"
	"github.com/Hunt4Bugs/agentsd/internal/ledger"
	"github.com/Hunt4Bugs/agentsd/internal/manifest"
)

func fixture(t *testing.T) (home, state string, cfg *config.Config, agents []*manifest.Resolved) {
	home = t.TempDir()
	state = filepath.Join(home, ".local", "state", "agentsd")
	cfg = config.Default()
	cfg.Roots = []string{filepath.Join(home, "src"), filepath.Join(home, "wiki"), filepath.Join(home, "out")}
	agents = []*manifest.Resolved{{
		Name: "researcher", Runtime: "claude-code", Timeout: manifest.Duration(30 * time.Minute), MaxConcurrent: 1,
		Read: []string{filepath.Join(home, "src")}, Write: []string{filepath.Join(home, "wiki", "research")},
		EnvPass: []string{}, EnvSet: map[string]string{}, Args: []string{}, OutRoot: filepath.Join(home, "out", "researcher"),
	}}
	return
}

func TestPlanApplyIdempotent(t *testing.T) {
	home, state, cfg, agents := fixture(t)
	l, _ := ledger.Load(state)
	p := Compute(cfg, agents, l)
	out := p.Render(home)
	for _, want := range []string{"+ create dir   ~/wiki/research", "~ register     agent researcher", "(new)", "Plan: 5 to create, 1 to register."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if err := Apply(p, l, state); err != nil {
		t.Fatal(err)
	}
	l, _ = ledger.Load(state)
	if p2 := Compute(cfg, agents, l); !p2.Empty() {
		t.Fatalf("not idempotent:\n%s", p2.Render(home))
	}
}

func TestChangedAndUnregister(t *testing.T) {
	_, state, cfg, agents := fixture(t)
	l, _ := ledger.Load(state)
	Apply(Compute(cfg, agents, l), l, state)
	changed := *agents[0]
	changed.Timeout = manifest.Duration(20 * time.Minute)
	p := Compute(cfg, []*manifest.Resolved{&changed}, l)
	if len(p.Actions) != 1 || p.Actions[0].Diff[0] != "limits.timeout 30m → 20m" {
		t.Fatalf("%+v", p.Actions)
	}
	p = Compute(cfg, nil, l)
	var kinds []string
	for _, a := range p.Actions {
		name := a.Agent
		if a.Path != "" {
			name = filepath.Base(a.Path)
		}
		kinds = append(kinds, string(a.Kind)+":"+name)
	}
	// wiki/research and out/researcher are empty and ours; roots stay.
	if strings.Join(kinds, ",") != "remove_dir:research,remove_dir:researcher,unregister:researcher" {
		t.Fatal(kinds)
	}
}

func TestNeverDeletesNonEmptyOrUnmanaged(t *testing.T) {
	home, state, cfg, agents := fixture(t)
	os.MkdirAll(filepath.Join(home, "out", "researcher"), 0o755) // pre-existing: not ours
	l, _ := ledger.Load(state)
	Apply(Compute(cfg, agents, l), l, state)
	os.WriteFile(filepath.Join(home, "wiki", "research", "notes.md"), []byte("x"), 0o644)
	l, _ = ledger.Load(state)
	p := Compute(cfg, nil, l)
	for _, a := range p.Actions {
		if a.Kind == RemoveDir {
			t.Fatalf("unexpected removal %s", a.Path)
		}
	}
	if err := Apply(p, l, state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "out", "researcher")); err != nil {
		t.Fatal("unmanaged dir removed")
	}
}
