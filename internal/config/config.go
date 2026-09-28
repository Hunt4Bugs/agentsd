// Package config loads and validates the global config.toml (spec §5).
package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Hunt4Bugs/agentsd/internal/diag"
	"github.com/Hunt4Bugs/agentsd/internal/paths"
	"github.com/Hunt4Bugs/agentsd/internal/tomlx"
)

type Config struct {
	Version  int                `toml:"version" json:"version"`
	Daemon   Daemon             `toml:"daemon" json:"daemon"`
	AHS      AHS                `toml:"ahs" json:"ahs"`
	Policy   Policy             `toml:"policy" json:"policy"`
	Runs     Runs               `toml:"runs" json:"runs"`
	Runtimes map[string]Runtime `toml:"runtimes" json:"runtimes"`

	// Resolved values (not decoded).
	Path  string   `toml:"-" json:"path"`
	Found bool     `toml:"-" json:"found"`
	Roots []string `toml:"-" json:"resolved_roots"`
}

type Daemon struct {
	LogLevel          string `toml:"log_level" json:"log_level"`
	MaxConcurrentRuns int    `toml:"max_concurrent_runs" json:"max_concurrent_runs"`
}

type AHS struct {
	Roots              []string `toml:"roots" json:"roots"`
	CreateMissingRoots bool     `toml:"create_missing_roots" json:"create_missing_roots"`
}

type Policy struct {
	Enforcement string `toml:"enforcement" json:"enforcement"`
}

type Runs struct {
	Retain     int `toml:"retain" json:"retain"`
	RetainDays int `toml:"retain_days" json:"retain_days"`
}

// Runtime overrides how an adapter invokes its runtime. Argv is a template;
// see package adapter for placeholders.
type Runtime struct {
	Command string   `toml:"command" json:"command"`
	Argv    []string `toml:"argv" json:"argv,omitempty"`
}

// ConfigurableRuntimes are the adapters that accept a [runtimes.<name>] table.
var ConfigurableRuntimes = []string{"claude-code", "codex"}

var LogLevels = []string{"error", "warn", "info", "debug", "trace"}

// Default returns the built-in defaults.
func Default() *Config {
	return &Config{
		Version: 1,
		Daemon:  Daemon{LogLevel: "info", MaxConcurrentRuns: 4},
		AHS:     AHS{Roots: []string{"~/src", "~/wiki", "~/out"}, CreateMissingRoots: true},
		Policy:  Policy{Enforcement: "advisory"},
		Runs:    Runs{Retain: 500, RetainDays: 90},
		Runtimes: map[string]Runtime{
			"claude-code": {Command: "claude"},
			"codex":       {Command: "codex"},
		},
	}
}

// Load reads path (a missing file yields defaults) and validates it. home is
// used for ~ expansion. The returned config is usable even when diagnostics
// contain errors, but callers should not act on it in that case.
func Load(path, home string) (*Config, diag.List) {
	c := Default()
	c.Path = path
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		c.resolveRoots(home, nil)
		return c, nil
	}
	if err != nil {
		return c, diag.List{{File: path, Severity: diag.SevError, Message: err.Error()}}
	}
	c.Found = true
	return Parse(path, data, home)
}

// Parse decodes and validates config data.
func Parse(path string, data []byte, home string) (*Config, diag.List) {
	c := Default()
	c.Path, c.Found = path, true
	defaults := c.Runtimes
	c.Runtimes = nil
	doc, diags := tomlx.Decode(path, data, c)
	for name, rt := range defaults {
		cur, ok := c.Runtimes[name]
		if !ok {
			if c.Runtimes == nil {
				c.Runtimes = map[string]Runtime{}
			}
			c.Runtimes[name] = rt
			continue
		}
		if cur.Command == "" {
			cur.Command = rt.Command
			c.Runtimes[name] = cur
		}
	}
	if diags.HasErrors() {
		return c, diags
	}
	diags = append(diags, tomlx.ReservedSecretRefs(doc)...)
	diags = append(diags, c.validate(doc)...)
	c.resolveRoots(home, doc)
	return c, diags
}

func (c *Config) resolveRoots(home string, _ *tomlx.Doc) {
	c.Roots = c.Roots[:0]
	for _, r := range c.AHS.Roots {
		if p, err := paths.Expand(r, home); err == nil {
			c.Roots = append(c.Roots, p)
		}
	}
}

func (c *Config) validate(doc *tomlx.Doc) diag.List {
	var out diag.List
	if !doc.Has("version") {
		out = append(out, doc.Err("version", "is required and must be 1"))
	} else if c.Version != 1 {
		out = append(out, doc.Err("version", "unsupported config version %d (want 1)", c.Version))
	}
	if !slices.Contains(LogLevels, c.Daemon.LogLevel) {
		out = append(out, doc.Err("daemon.log_level", "must be one of %s", strings.Join(LogLevels, ", ")))
	}
	if c.Daemon.MaxConcurrentRuns < 1 {
		out = append(out, doc.Err("daemon.max_concurrent_runs", "must be at least 1"))
	}
	if len(c.AHS.Roots) == 0 {
		out = append(out, doc.Err("ahs.roots", "must list at least one root"))
	}
	for _, r := range c.AHS.Roots {
		if _, err := paths.Expand(r, "/"); err != nil {
			out = append(out, doc.Err("ahs.roots", "%v", err))
		}
	}
	if c.Policy.Enforcement != "advisory" {
		out = append(out, doc.Err("policy.enforcement", "v0.1 supports only \"advisory\" (got %q)", c.Policy.Enforcement))
	}
	if c.Runs.Retain < 0 {
		out = append(out, doc.Err("runs.retain", "must not be negative"))
	}
	if c.Runs.RetainDays < 0 {
		out = append(out, doc.Err("runs.retain_days", "must not be negative"))
	}
	for name, rt := range c.Runtimes {
		key := "runtimes." + name
		if !slices.Contains(ConfigurableRuntimes, name) {
			out = append(out, doc.Err(key, "unknown runtime (configurable: %s)", strings.Join(ConfigurableRuntimes, ", ")))
			continue
		}
		if strings.TrimSpace(rt.Command) == "" {
			out = append(out, doc.Err(key+".command", "must not be empty"))
		}
		if len(rt.Argv) > 0 {
			if rt.Argv[0] != "{command}" {
				out = append(out, doc.Err(key+".argv", "first element must be \"{command}\""))
			}
			for _, a := range rt.Argv {
				if strings.Contains(a, "{") && !slices.Contains(Placeholders, a) {
					out = append(out, doc.Err(key+".argv", "unknown placeholder %q (known: %s)", a, strings.Join(Placeholders, ", ")))
				}
			}
		}
	}
	return out
}

// Placeholders allowed in a runtime argv template. Each must be a whole element.
var Placeholders = []string{"{command}", "{prompt}", "{args...}"}

// AgentsDir is the manifest directory that sits beside config.toml.
func AgentsDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "agents")
}
