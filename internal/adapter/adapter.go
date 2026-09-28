// Package adapter turns a resolved manifest, a prompt and a run context into a
// process spec (spec §7). Adapters are compiled in for v0.1.
package adapter

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"slices"
	"sort"

	"github.com/Hunt4Bugs/agentsd/internal/config"
	"github.com/Hunt4Bugs/agentsd/internal/manifest"
	"github.com/Hunt4Bugs/agentsd/internal/xdg"
)

// DefaultArgv are the built-in argv templates. {prompt} is replaced by the
// prompt text, or handled per adapter when the prompt arrives on stdin.
var DefaultArgv = map[string][]string{
	"claude-code": {"{command}", "-p", "{prompt}", "{args...}"},
	"codex":       {"{command}", "exec", "{prompt}", "{args...}"},
}

// stdinPromptArg is what {prompt} becomes when the prompt is piped on stdin:
// claude -p reads stdin when no prompt argument is given; codex exec takes "-".
var stdinPromptArg = map[string]string{
	"claude-code": "",
	"codex":       "-",
}

// RunContext is the per-run information injected into the process.
type RunContext struct {
	RunID       string
	OutDir      string
	Cwd         string
	Prompt      string
	PromptStdin bool
	Dirs        xdg.Dirs
	// LookupEnv reads the daemon's environment (os.LookupEnv by default).
	LookupEnv func(string) (string, bool)
}

// ProcSpec is everything needed to start the process.
type ProcSpec struct {
	Argv []string
	Env  []string
	Dir  string
	// Stdin is piped to the process when HasStdin; otherwise stdin is /dev/null.
	Stdin    string
	HasStdin bool
	// EnvPassed are the env.pass names found in the daemon's environment;
	// EnvMissing were requested but absent. Values are never recorded.
	EnvPassed  []string
	EnvMissing []string
}

// Command returns the executable an agent needs (before PATH lookup).
func Command(r *manifest.Resolved, cfg *config.Config) string {
	if r.Runtime == "exec" {
		if len(r.Command) == 0 {
			return ""
		}
		return r.Command[0]
	}
	return cfg.Runtimes[r.Runtime].Command
}

// Available resolves the agent's executable on PATH.
func Available(r *manifest.Resolved, cfg *config.Config) (string, error) {
	cmd := Command(r, cfg)
	if cmd == "" {
		return "", fmt.Errorf("no command configured for runtime %q", r.Runtime)
	}
	p, err := exec.LookPath(cmd)
	if err != nil {
		return "", fmt.Errorf("runtime %s: %q not found on PATH", r.Runtime, cmd)
	}
	return p, nil
}

// Build produces the process spec for one run.
func Build(r *manifest.Resolved, cfg *config.Config, rc RunContext) (ProcSpec, error) {
	if rc.LookupEnv == nil {
		rc.LookupEnv = os.LookupEnv
	}
	spec := ProcSpec{Dir: rc.Cwd}
	switch r.Runtime {
	case "exec":
		if len(r.Command) == 0 {
			return spec, fmt.Errorf("exec agent %s has no runtime_options.command", r.Name)
		}
		spec.Argv = append(slices.Clone(r.Command), r.Args...)
		spec.Stdin, spec.HasStdin = rc.Prompt, true
	case "claude-code", "codex":
		rt := cfg.Runtimes[r.Runtime]
		tmpl := rt.Argv
		if len(tmpl) == 0 {
			tmpl = DefaultArgv[r.Runtime]
		}
		hasPrompt := slices.Contains(tmpl, "{prompt}")
		for _, a := range tmpl {
			switch a {
			case "{command}":
				spec.Argv = append(spec.Argv, rt.Command)
			case "{args...}":
				spec.Argv = append(spec.Argv, r.Args...)
			case "{prompt}":
				if !rc.PromptStdin {
					spec.Argv = append(spec.Argv, rc.Prompt)
				} else if s := stdinPromptArg[r.Runtime]; s != "" {
					spec.Argv = append(spec.Argv, s)
				}
			default:
				spec.Argv = append(spec.Argv, a)
			}
		}
		if rc.PromptStdin || !hasPrompt {
			spec.Stdin, spec.HasStdin = rc.Prompt, true
		}
	default:
		return spec, fmt.Errorf("unknown runtime %q", r.Runtime)
	}
	spec.Env, spec.EnvPassed, spec.EnvMissing = buildEnv(r, rc)
	return spec, nil
}

// baseEnv is inherited from the daemon when present (spec §7.1).
var baseEnv = []string{"PATH", "HOME", "USER", "LANG", "TERM"}

func buildEnv(r *manifest.Resolved, rc RunContext) (env, passed, missing []string) {
	m := map[string]string{}
	for _, k := range baseEnv {
		if v, ok := rc.LookupEnv(k); ok {
			m[k] = v
		}
	}
	if _, ok := m["HOME"]; !ok && rc.Dirs.Home != "" {
		m["HOME"] = rc.Dirs.Home
	}
	if _, ok := m["USER"]; !ok {
		if u, err := user.Current(); err == nil {
			m["USER"] = u.Username
		}
	}
	if _, ok := m["PATH"]; !ok {
		m["PATH"] = "/usr/bin:/bin:/usr/sbin:/sbin"
	}
	m["XDG_CONFIG_HOME"] = rc.Dirs.ConfigHome
	m["XDG_STATE_HOME"] = rc.Dirs.StateHome
	m["XDG_DATA_HOME"] = rc.Dirs.DataHome
	m["XDG_CACHE_HOME"] = rc.Dirs.CacheHome
	if rc.Dirs.RuntimeDir != "" {
		m["XDG_RUNTIME_DIR"] = rc.Dirs.RuntimeDir
	}
	for _, k := range r.EnvPass {
		if v, ok := rc.LookupEnv(k); ok {
			m[k] = v
			passed = append(passed, k)
		} else {
			missing = append(missing, k)
		}
	}
	for k, v := range r.EnvSet {
		m[k] = v
	}
	m["AGENTSD_RUN_ID"] = rc.RunID
	m["AGENTSD_AGENT"] = r.Name
	m["AGENTSD_OUT"] = rc.OutDir
	if r.Runtime == "exec" {
		m["AGENTSD_PROMPT"] = rc.Prompt
	}
	for k, v := range m {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return env, passed, missing
}
