// Package manifest loads, validates and resolves agent manifests (spec §6).
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Hunt4Bugs/agentsd/internal/config"
	"github.com/Hunt4Bugs/agentsd/internal/diag"
	"github.com/Hunt4Bugs/agentsd/internal/paths"
	"github.com/Hunt4Bugs/agentsd/internal/tomlx"
	"github.com/Hunt4Bugs/agentsd/internal/xdg"
)

// Manifest is the on-disk shape of agents/<name>.toml.
type Manifest struct {
	Name        string         `toml:"name"`
	Description string         `toml:"description"`
	Runtime     string         `toml:"runtime"`
	Workspace   Workspace      `toml:"workspace"`
	Limits      Limits         `toml:"limits"`
	Env         Env            `toml:"env"`
	Options     RuntimeOptions `toml:"runtime_options"`
}

type Workspace struct {
	Cwd               string   `toml:"cwd"`
	Read              []string `toml:"read"`
	Write             []string `toml:"write"`
	AllowOutsideRoots bool     `toml:"allow_outside_roots"`
}

type Limits struct {
	Timeout       string `toml:"timeout"`
	MaxConcurrent int    `toml:"max_concurrent"`
}

type Env struct {
	Pass []string          `toml:"pass"`
	Set  map[string]string `toml:"set"`
}

// RuntimeOptions is the [runtime_options] table. (The spec's examples used
// [runtime], which is invalid TOML alongside the runtime = "..." key.)
type RuntimeOptions struct {
	Command []string `toml:"command"`
	Args    []string `toml:"args"`
}

// Runtimes are the built-in adapters.
var Runtimes = []string{"claude-code", "codex", "exec"}

const (
	DefaultTimeout = 30 * time.Minute
	MaxTimeout     = 24 * time.Hour
)

var (
	nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	envRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Duration is a time.Duration that marshals as a Go duration string.
type Duration time.Duration

// String formats like time.Duration but drops zero trailing units (30m, 2h).
func (d Duration) String() string {
	s := time.Duration(d).String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	*d = Duration(v)
	return err
}

// Resolved is a validated manifest with paths expanded and defaults applied.
type Resolved struct {
	Name              string            `json:"name" toml:"name"`
	Description       string            `json:"description,omitempty" toml:"description,omitempty"`
	Runtime           string            `json:"runtime" toml:"runtime"`
	File              string            `json:"file" toml:"file"`
	Cwd               string            `json:"cwd" toml:"cwd"` // empty: the run's output directory
	Read              []string          `json:"read" toml:"read"`
	Write             []string          `json:"write" toml:"write"`
	AllowOutsideRoots bool              `json:"allow_outside_roots" toml:"allow_outside_roots"`
	Timeout           Duration          `json:"timeout" toml:"timeout"`
	MaxConcurrent     int               `json:"max_concurrent" toml:"max_concurrent"`
	EnvPass           []string          `json:"env_pass" toml:"env_pass"`
	EnvSet            map[string]string `json:"env_set" toml:"env_set"`
	Command           []string          `json:"command,omitempty" toml:"command,omitempty"`
	Args              []string          `json:"args" toml:"args"`
	OutRoot           string            `json:"out_root" toml:"out_root"`
}

// Context carries what resolution needs from the environment.
type Context struct {
	Home   string
	Dirs   xdg.Dirs
	Config *config.Config
}

// AllowedRoots are the AHS roots plus the XDG base directories.
func (c Context) AllowedRoots() []string {
	roots := slices.Clone(c.Config.Roots)
	return append(roots, c.Dirs.ConfigHome, c.Dirs.StateHome, c.Dirs.DataHome, c.Dirs.CacheHome)
}

// OutRoot is ~/out; per-run output lives at OutRoot/<agent>/<run-id>.
func (c Context) OutRoot() string { return filepath.Join(c.Home, "out") }

// LoadFile parses and validates one manifest file.
func LoadFile(path string, ctx Context) (*Resolved, diag.List) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, diag.List{{File: path, Severity: diag.SevError, Message: err.Error()}}
	}
	return Parse(path, data, ctx)
}

// Parse validates manifest data. path's stem must equal the manifest name.
func Parse(path string, data []byte, ctx Context) (*Resolved, diag.List) {
	var m Manifest
	doc, diags := tomlx.Decode(path, data, &m)
	if diags.HasErrors() {
		return nil, diags
	}
	diags = append(diags, tomlx.ReservedSecretRefs(doc)...)
	r := &Resolved{
		Name: m.Name, Description: m.Description, Runtime: m.Runtime, File: path,
		AllowOutsideRoots: m.Workspace.AllowOutsideRoots,
		Timeout:           Duration(DefaultTimeout), MaxConcurrent: 1,
		EnvPass: nonNil(m.Env.Pass), EnvSet: m.Env.Set,
		Command: m.Options.Command, Args: nonNil(m.Options.Args),
		OutRoot: filepath.Join(ctx.OutRoot(), m.Name),
	}
	if r.EnvSet == nil {
		r.EnvSet = map[string]string{}
	}

	switch {
	case !doc.Has("name"):
		diags = append(diags, doc.Err("name", "is required"))
	case !nameRE.MatchString(m.Name):
		diags = append(diags, doc.Err("name", "%q must match %s", m.Name, nameRE))
	case strings.TrimSuffix(filepath.Base(path), ".toml") != m.Name:
		diags = append(diags, doc.Err("name", "%q does not match file name %s", m.Name, filepath.Base(path)))
	}

	if !slices.Contains(Runtimes, m.Runtime) {
		diags = append(diags, doc.Err("runtime", "must be one of %s (got %q)", strings.Join(Runtimes, ", "), m.Runtime))
	}
	if m.Runtime == "exec" && len(m.Options.Command) == 0 {
		diags = append(diags, doc.Err("runtime_options.command", "is required for exec agents"))
	}
	if m.Runtime != "exec" && doc.Has("runtime_options.command") {
		diags = append(diags, doc.Err("runtime_options.command", "is only valid for exec agents; configure other runtimes under [runtimes.%s] in config.toml", m.Runtime))
	}

	if m.Limits.Timeout != "" {
		d, err := time.ParseDuration(m.Limits.Timeout)
		switch {
		case err != nil:
			diags = append(diags, doc.Err("limits.timeout", "%q is not a duration (e.g. 90s, 30m, 2h)", m.Limits.Timeout))
		case d <= 0:
			diags = append(diags, doc.Err("limits.timeout", "must be positive"))
		case d > MaxTimeout:
			diags = append(diags, doc.Err("limits.timeout", "%s exceeds the 24h maximum", d))
		default:
			r.Timeout = Duration(d)
		}
	}
	if doc.Has("limits.max_concurrent") {
		if m.Limits.MaxConcurrent < 1 {
			diags = append(diags, doc.Err("limits.max_concurrent", "must be at least 1"))
		} else {
			r.MaxConcurrent = m.Limits.MaxConcurrent
		}
	}

	for _, name := range m.Env.Pass {
		if !envRE.MatchString(name) {
			diags = append(diags, doc.Err("env.pass", "%q is not a valid variable name", name))
		}
	}
	for k := range m.Env.Set {
		key := "env.set." + k
		switch {
		case !envRE.MatchString(k):
			diags = append(diags, doc.Err(key, "%q is not a valid variable name", k))
		case strings.HasPrefix(k, "AGENTSD_"):
			diags = append(diags, doc.Err(key, "AGENTSD_* variables are reserved"))
		}
	}

	roots := ctx.AllowedRoots()
	resolve := func(key, p string) (string, bool) {
		abs, err := paths.Expand(p, ctx.Home)
		if err != nil {
			diags = append(diags, doc.Err(key, "%v", err))
			return "", false
		}
		if !m.Workspace.AllowOutsideRoots && !paths.WithinAny(abs, roots) {
			diags = append(diags, doc.Err(key, "%s is outside the AHS roots and XDG directories (set workspace.allow_outside_roots to permit)", p))
			return "", false
		}
		return abs, true
	}
	for _, p := range m.Workspace.Read {
		if abs, ok := resolve("workspace.read", p); ok {
			r.Read = append(r.Read, abs)
		}
	}
	for _, p := range m.Workspace.Write {
		if abs, ok := resolve("workspace.write", p); ok {
			r.Write = append(r.Write, abs)
		}
	}
	r.Read, r.Write = nonNil(r.Read), nonNil(r.Write)
	if m.Workspace.Cwd != "" {
		if abs, ok := resolve("workspace.cwd", m.Workspace.Cwd); ok {
			r.Cwd = abs
			if !r.CwdAllowed(abs) {
				diags = append(diags, doc.Err("workspace.cwd", "%s is not under workspace.read or workspace.write", m.Workspace.Cwd))
			}
		}
	}
	if diags.HasErrors() {
		return nil, diags
	}
	return r, diags
}

// CwdAllowed implements policy rule 1: cwd must fall within read ∪ write (or
// the agent's output root, which is always writable).
func (r *Resolved) CwdAllowed(cwd string) bool {
	return paths.WithinAny(cwd, append(append(slices.Clone(r.Read), r.Write...), r.OutRoot))
}

// LoadDir loads every *.toml manifest in dir. Invalid manifests are reported
// and omitted. A missing directory yields no agents.
func LoadDir(dir string, ctx Context) ([]*Resolved, diag.List) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, diag.List{{File: dir, Severity: diag.SevError, Message: err.Error()}}
	}
	var out []*Resolved
	var diags diag.List
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		r, d := LoadFile(filepath.Join(dir, e.Name()), ctx)
		diags = append(diags, d...)
		if r != nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, diags
}

// Flatten renders the resolved manifest as manifest-style keys for diffing.
func (r *Resolved) Flatten() map[string]string {
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	return map[string]string{
		"description":                   strconv.Quote(r.Description),
		"runtime":                       r.Runtime,
		"workspace.cwd":                 r.Cwd,
		"workspace.read":                j(r.Read),
		"workspace.write":               j(r.Write),
		"workspace.allow_outside_roots": strconv.FormatBool(r.AllowOutsideRoots),
		"limits.timeout":                r.Timeout.String(),
		"limits.max_concurrent":         strconv.Itoa(r.MaxConcurrent),
		"env.pass":                      j(r.EnvPass),
		"env.set":                       j(r.EnvSet),
		"runtime_options.command":       j(r.Command),
		"runtime_options.args":          j(r.Args),
	}
}

// Diff lists changed fields from old to new as "key old → new", sorted.
func Diff(old, new *Resolved) []string {
	a, b := old.Flatten(), new.Flatten()
	var out []string
	for k, nv := range b {
		if ov := a[k]; ov != nv {
			out = append(out, fmt.Sprintf("%s %s → %s", k, short(ov), short(nv)))
		}
	}
	sort.Strings(out)
	return out
}

func short(s string) string {
	if s == "" {
		return `""`
	}
	if len(s) > 40 {
		return s[:37] + "..."
	}
	return s
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
