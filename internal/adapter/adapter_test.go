package adapter

import (
	"slices"
	"strings"
	"testing"

	"github.com/Hunt4Bugs/agentsd/internal/config"
	"github.com/Hunt4Bugs/agentsd/internal/manifest"
	"github.com/Hunt4Bugs/agentsd/internal/xdg"
)

func rc(prompt string, stdin bool) RunContext {
	env := map[string]string{"PATH": "/bin", "HOME": "/h", "SECRET": "s3cr3t", "ANTHROPIC_API_KEY": "k", "EDITOR": "vim"}
	return RunContext{
		RunID: "01J", OutDir: "/h/out/a/01J", Cwd: "/h/src", Prompt: prompt, PromptStdin: stdin,
		Dirs:      xdg.FromEnv(func(string) string { return "" }, "/h"),
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	}
}

func agent(runtime string) *manifest.Resolved {
	return &manifest.Resolved{Name: "a", Runtime: runtime, Args: []string{"--model", "x"},
		EnvPass: []string{"ANTHROPIC_API_KEY", "NOPE"}, EnvSet: map[string]string{"MODE": "deep"}}
}

func TestClaudeArgvAndStdin(t *testing.T) {
	cfg := config.Default()
	s, err := Build(agent("claude-code"), cfg, rc("hello", false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Argv, " ") != "claude -p hello --model x" || s.HasStdin {
		t.Fatalf("%q", s.Argv)
	}
	s, _ = Build(agent("claude-code"), cfg, rc("hello", true))
	if strings.Join(s.Argv, " ") != "claude -p --model x" || !s.HasStdin || s.Stdin != "hello" {
		t.Fatalf("%q", s.Argv)
	}
	s, _ = Build(agent("codex"), cfg, rc("hello", true))
	if strings.Join(s.Argv, " ") != "codex exec - --model x" || s.Stdin != "hello" {
		t.Fatalf("%q", s.Argv)
	}
}

func TestArgvOverride(t *testing.T) {
	cfg := config.Default()
	cfg.Runtimes["codex"] = config.Runtime{Command: "/opt/codex", Argv: []string{"{command}", "run", "--quiet", "{args...}", "{prompt}"}}
	s, _ := Build(agent("codex"), cfg, rc("p", false))
	if strings.Join(s.Argv, " ") != "/opt/codex run --quiet --model x p" {
		t.Fatalf("%q", s.Argv)
	}
}

func TestExec(t *testing.T) {
	a := agent("exec")
	a.Command = []string{"/bin/sh", "-c", "cat"}
	s, _ := Build(a, config.Default(), rc("p", false))
	if strings.Join(s.Argv, " ") != "/bin/sh -c cat --model x" || !s.HasStdin || s.Stdin != "p" {
		t.Fatalf("%+v", s)
	}
	if !slices.Contains(s.Env, "AGENTSD_PROMPT=p") {
		t.Fatal("missing AGENTSD_PROMPT")
	}
}

func TestEnvMinimal(t *testing.T) {
	s, _ := Build(agent("claude-code"), config.Default(), rc("p", false))
	joined := strings.Join(s.Env, "\n")
	for _, want := range []string{"PATH=/bin", "HOME=/h", "ANTHROPIC_API_KEY=k", "MODE=deep", "AGENTSD_RUN_ID=01J",
		"AGENTSD_AGENT=a", "AGENTSD_OUT=/h/out/a/01J", "XDG_CONFIG_HOME=/h/.config"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, bad := range []string{"SECRET", "EDITOR", "AGENTSD_PROMPT"} {
		if strings.Contains(joined, bad+"=") {
			t.Errorf("leaked %s", bad)
		}
	}
	if !slices.Equal(s.EnvPassed, []string{"ANTHROPIC_API_KEY"}) || !slices.Equal(s.EnvMissing, []string{"NOPE"}) {
		t.Fatalf("passed=%v missing=%v", s.EnvPassed, s.EnvMissing)
	}
}
