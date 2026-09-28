package service

import (
	"strings"
	"testing"
)

func TestRenderPlist(t *testing.T) {
	l := &Launchd{Home: "/Users/u", LogDir: "/Users/u/.local/state/agentsd", UID: 501}
	out := l.Render(InstallOptions{Binary: "/opt/homebrew/bin/agentsd", Env: map[string]string{"PATH": "/a:/b&c"}})
	for _, want := range []string{"<string>dev.agentsd.daemon</string>", "<string>/opt/homebrew/bin/agentsd</string>\n    <string>daemon</string>",
		"<key>PATH</key>\n    <string>/a:/b&amp;c</string>", "launchd.log"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if l.Path() != "/Users/u/Library/LaunchAgents/dev.agentsd.daemon.plist" {
		t.Fatal(l.Path())
	}
	withEnv := l.Render(InstallOptions{Binary: "/bin/agentsd", EnvFile: "/Users/u/.config/agentsd/secrets.env"})
	if !strings.Contains(withEnv, "<string>/bin/sh</string>") || !strings.Contains(withEnv, "secrets.env") {
		t.Fatal(withEnv)
	}
}

func TestRenderUnit(t *testing.T) {
	s := &Systemd{Dir: "/home/u/.config/systemd/user"}
	out := s.Render(InstallOptions{Binary: "/home/u/.local/bin/agentsd", EnvFile: "/home/u/.config/agentsd/env", Env: map[string]string{"PATH": "/usr/bin", "X": `a"b%`}})
	for _, want := range []string{"ExecStart=/home/u/.local/bin/agentsd daemon", `Environment="PATH=/usr/bin"`, `Environment="X=a\"b%%"`,
		"EnvironmentFile=/home/u/.config/agentsd/env", "WantedBy=default.target", "KillMode=mixed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestLaunchdStatusParse(t *testing.T) {
	home := t.TempDir()
	out := "gui/501/dev.agentsd.daemon = {\n\tpath = " + home + "/Library/LaunchAgents/dev.agentsd.daemon.plist\n\tstate = running\n\tpid = 4121\n}"
	l := &Launchd{Home: home, UID: 501, Run: func(string, ...string) (string, error) { return out, nil }}
	st := l.Status()
	if !st.Loaded || !st.Running || st.PID != 4121 || st.Installed {
		t.Fatalf("%+v", st)
	}
	other := &Launchd{Home: t.TempDir(), UID: 501, Run: func(string, ...string) (string, error) { return out, nil }}
	if st := other.Status(); st.Loaded || st.Running {
		t.Fatalf("job from another plist treated as ours: %+v", st)
	}
}
