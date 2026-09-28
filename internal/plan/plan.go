// Package plan computes and applies the difference between the manifests and
// the machine (spec §10 plan/apply).
package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/config"
	"github.com/Hunt4Bugs/agentsd/internal/ledger"
	"github.com/Hunt4Bugs/agentsd/internal/manifest"
	"github.com/Hunt4Bugs/agentsd/internal/paths"
)

type Kind string

const (
	CreateDir  Kind = "create_dir"
	RemoveDir  Kind = "remove_dir"
	Register   Kind = "register"
	Unregister Kind = "unregister"
)

type Action struct {
	Kind   Kind     `json:"kind"`
	Path   string   `json:"path,omitempty"`
	Agent  string   `json:"agent,omitempty"`
	Reason string   `json:"reason,omitempty"`
	Diff   []string `json:"diff,omitempty"`

	resolved *manifest.Resolved
}

type Plan struct {
	Actions []Action `json:"actions"`
}

func (p Plan) Empty() bool { return len(p.Actions) == 0 }

func (p Plan) count(k Kind) int {
	n := 0
	for _, a := range p.Actions {
		if a.Kind == k {
			n++
		}
	}
	return n
}

// Summary is the closing line of `plan` output.
func (p Plan) Summary() string {
	if p.Empty() {
		return "No changes. The machine matches the manifests."
	}
	var parts []string
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d to %s", n, what))
		}
	}
	add(p.count(CreateDir), "create")
	add(p.count(RemoveDir), "remove")
	add(p.count(Register), "register")
	add(p.count(Unregister), "unregister")
	return "Plan: " + strings.Join(parts, ", ") + "."
}

// Render formats the plan like the spec example.
func (p Plan) Render(home string) string {
	var b strings.Builder
	for _, a := range p.Actions {
		var sym, verb, subject, note string
		switch a.Kind {
		case CreateDir:
			sym, verb, subject, note = "+", "create dir", paths.Contract(a.Path, home), a.Reason
		case RemoveDir:
			sym, verb, subject, note = "-", "remove dir", paths.Contract(a.Path, home), a.Reason
		case Register:
			sym, verb, subject = "~", "register", "agent "+a.Agent
			if len(a.Diff) == 0 {
				note = "new"
			} else {
				note = "changed: " + strings.Join(a.Diff, "; ")
			}
		case Unregister:
			sym, verb, subject, note = "-", "unregister", "agent "+a.Agent, a.Reason
		}
		line := fmt.Sprintf("%s %-12s %-30s", sym, verb, subject)
		if note != "" {
			line += " (" + note + ")"
		}
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	if !p.Empty() {
		b.WriteString("\n")
	}
	b.WriteString(p.Summary() + "\n")
	return b.String()
}

type need struct {
	path   string
	reason string
}

// Compute diffs agents and config against the ledger and the filesystem.
func Compute(cfg *config.Config, agents []*manifest.Resolved, l *ledger.Ledger) Plan {
	var needs []need
	if cfg.AHS.CreateMissingRoots {
		for _, r := range cfg.Roots {
			needs = append(needs, need{r, "ahs root"})
		}
	}
	for _, a := range agents {
		needs = append(needs, need{a.OutRoot, ""})
		for _, w := range a.Write {
			needs = append(needs, need{w, a.Name + ": workspace.write"})
		}
	}

	var p Plan
	creating := map[string]bool{}
	for _, n := range needs {
		for _, d := range missingChain(n.path) {
			if creating[d] {
				continue
			}
			creating[d] = true
			reason := ""
			if d == n.path {
				reason = n.reason
			}
			p.Actions = append(p.Actions, Action{Kind: CreateDir, Path: d, Reason: reason})
		}
	}
	sort.SliceStable(p.Actions, func(i, j int) bool { return p.Actions[i].Path < p.Actions[j].Path })

	wanted := map[string]bool{}
	for _, n := range needs {
		for d := n.path; d != "/" && d != "."; d = filepath.Dir(d) {
			wanted[d] = true
		}
	}
	var removals []Action
	for _, d := range l.Dirs {
		if wanted[d.Path] || !isEmptyDir(d.Path) {
			continue
		}
		removals = append(removals, Action{Kind: RemoveDir, Path: d.Path, Reason: "no longer needed; empty and created by apply"})
	}
	sort.Slice(removals, func(i, j int) bool { return removals[i].Path > removals[j].Path })
	p.Actions = append(p.Actions, removals...)

	seen := map[string]bool{}
	for _, a := range agents {
		seen[a.Name] = true
		old, ok := l.Agents[a.Name]
		switch {
		case !ok:
			p.Actions = append(p.Actions, Action{Kind: Register, Agent: a.Name, resolved: a})
		default:
			if d := manifest.Diff(old, a); len(d) > 0 {
				p.Actions = append(p.Actions, Action{Kind: Register, Agent: a.Name, Diff: d, resolved: a})
			}
		}
	}
	var gone []string
	for name := range l.Agents {
		if !seen[name] {
			gone = append(gone, name)
		}
	}
	sort.Strings(gone)
	for _, name := range gone {
		p.Actions = append(p.Actions, Action{Kind: Unregister, Agent: name, Reason: "manifest removed"})
	}
	return p
}

// Apply performs the plan and records the results in the ledger. It never
// deletes a directory it did not create, and never a non-empty one. The
// ledger is saved even when an action fails part-way.
func Apply(p Plan, l *ledger.Ledger, stateDir string) (err error) {
	defer func() {
		if serr := l.Save(stateDir); err == nil {
			err = serr
		}
	}()
	// Forget ledger dirs that no longer exist.
	for _, d := range append([]ledger.Dir{}, l.Dirs...) {
		if !paths.Exists(d.Path) {
			l.RemoveDir(d.Path)
		}
	}
	for _, a := range p.Actions {
		switch a.Kind {
		case CreateDir:
			if paths.Exists(a.Path) {
				continue
			}
			if err := os.Mkdir(a.Path, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", a.Path, err)
			}
			l.Dirs = append(l.Dirs, ledger.Dir{Path: a.Path, Reason: a.Reason, CreatedAt: time.Now().UTC().Truncate(time.Second)})
		case RemoveDir:
			if !l.HasDir(a.Path) || !isEmptyDir(a.Path) {
				continue
			}
			if err := os.Remove(a.Path); err != nil {
				return fmt.Errorf("remove %s: %w", a.Path, err)
			}
			l.RemoveDir(a.Path)
		case Register:
			l.Agents[a.Agent] = a.resolved
		case Unregister:
			delete(l.Agents, a.Agent)
		}
	}
	return nil
}

// missingChain lists p and its missing ancestors, outermost first.
func missingChain(p string) []string {
	var chain []string
	for d := p; !paths.Exists(d); d = filepath.Dir(d) {
		chain = append([]string{d}, chain...)
		if filepath.Dir(d) == d {
			break
		}
	}
	return chain
}

func isEmptyDir(p string) bool {
	fi, err := os.Lstat(p)
	if err != nil || !fi.IsDir() {
		return false
	}
	entries, err := os.ReadDir(p)
	return err == nil && len(entries) == 0
}
