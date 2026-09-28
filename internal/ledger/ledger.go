// Package ledger reads and writes managed.toml: the record of every directory
// `apply` created and every agent it registered.
package ledger

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/Hunt4Bugs/agentsd/internal/manifest"
	"github.com/Hunt4Bugs/agentsd/internal/runstore"
)

const File = "managed.toml"

type Ledger struct {
	Version int                           `toml:"version"`
	Dirs    []Dir                         `toml:"dirs"`
	Agents  map[string]*manifest.Resolved `toml:"agents"`
}

type Dir struct {
	Path      string    `toml:"path"`
	Reason    string    `toml:"reason"`
	CreatedAt time.Time `toml:"created_at"`
}

func Path(stateDir string) string { return filepath.Join(stateDir, File) }

// Load reads the ledger; a missing file is an empty ledger.
func Load(stateDir string) (*Ledger, error) {
	l := &Ledger{Version: 1, Agents: map[string]*manifest.Resolved{}}
	b, err := os.ReadFile(Path(stateDir))
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := toml.Unmarshal(b, l); err != nil {
		return nil, err
	}
	if l.Agents == nil {
		l.Agents = map[string]*manifest.Resolved{}
	}
	for _, a := range l.Agents {
		if a.EnvSet == nil {
			a.EnvSet = map[string]string{}
		}
	}
	return l, nil
}

// Save writes the ledger atomically.
func (l *Ledger) Save(stateDir string) error {
	sort.Slice(l.Dirs, func(i, j int) bool { return l.Dirs[i].Path < l.Dirs[j].Path })
	var buf bytes.Buffer
	buf.WriteString("# Managed by `agentsd apply`. Do not edit.\n")
	enc := toml.NewEncoder(&buf)
	enc.SetIndentTables(true)
	if err := enc.Encode(l); err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return runstore.WriteFileAtomic(Path(stateDir), buf.Bytes(), 0o600)
}

// HasDir reports whether p was created by apply.
func (l *Ledger) HasDir(p string) bool {
	for _, d := range l.Dirs {
		if d.Path == p {
			return true
		}
	}
	return false
}

// RemoveDir forgets p.
func (l *Ledger) RemoveDir(p string) {
	out := l.Dirs[:0]
	for _, d := range l.Dirs {
		if d.Path != p {
			out = append(out, d)
		}
	}
	l.Dirs = out
}

// Registered returns the registered agents sorted by name.
func (l *Ledger) Registered() []*manifest.Resolved {
	out := make([]*manifest.Resolved, 0, len(l.Agents))
	for _, a := range l.Agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
