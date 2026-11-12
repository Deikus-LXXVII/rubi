package home

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

func TestLoginHelperMatchesUpstream(t *testing.T) {
	upstream, err := os.ReadFile(filepath.Join("..", "..", "runner", "container", "rubi-claude-login"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(loginHelperSource), bytes.TrimSpace(upstream)) {
		t.Error("embedded sign-in helper is not the same as runner/container/rubi-claude-login")
	}
}

func TestNativeLoginLaunchPolicy(t *testing.T) {
	l := newLife(t)
	proj := l.proj
	cli := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	srt := "/usr/bin/true"
	if err := l.h.Update(func(c *Config) {
		c.AgentSandbox = agentsandbox.Installation{
			SRT:        srt,
			SearchPath: []string{"/usr/bin", "/bin"},
			Providers:  map[string]agentsandbox.Provider{"claudeAgent": {CLI: cli}},
		}
	}); err != nil {
		t.Fatal(err)
	}
	helper, err := l.t3.prepareLoginHelper(cli)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(src, []byte("const CLAUDE = "+quoteJSON(cli))) {
		t.Fatal("prepared helper does not name the Claude CLI")
	}
	if bytes.Contains(src, []byte("/opt/rubi-runner")) {
		t.Fatal("prepared helper still names the container Claude path")
	}

	launch, err := l.t3.nativeLoginLaunch(helper, cli)
	if err != nil {
		t.Fatal(err)
	}
	want, err := agent.EnsureSignInDir(agent.SignInDirOf(l.h.Dir))
	if err != nil {
		t.Fatal(err)
	}
	if launch.Policy.Login != want {
		t.Fatalf("login %q, want shared dir %q", launch.Policy.Login, want)
	}
	if launch.Provider != "claudeAgent" || launch.CLI != helper {
		t.Fatalf("launch cli: %s %s", launch.Provider, launch.CLI)
	}
	if got := launch.Policy.AllowedDomains; len(got) != len(providerSignInHosts) {
		t.Fatalf("hosts %v", got)
	}
	for i, h := range providerSignInHosts {
		if launch.Policy.AllowedDomains[i] != h {
			t.Fatalf("hosts %v", launch.Policy.AllowedDomains)
		}
	}
	for _, p := range launch.Policy.Projects {
		if p == proj || agent.Within(p, proj) || agent.Within(proj, p) {
			t.Fatal("a user project is mounted in the sign-in sandbox")
		}
	}
	for _, s := range launch.Policy.SensitiveRoots {
		if s == proj || s == l.h.Dir {
			// expected: helper dir and projects are hidden
			continue
		}
	}
	hidden := false
	for _, s := range launch.Policy.SensitiveRoots {
		if s == proj || s == l.h.Dir {
			hidden = true
		}
	}
	if !hidden {
		t.Fatal("helper dir or project is not in SensitiveRoots")
	}

	settings, err := agentsandbox.Build(launch.Policy, launch.Protected)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range settings.Filesystem.AllowWrite {
		if w == proj || agent.Within(proj, w) {
			t.Fatal("sign-in can write a user project")
		}
		for _, prot := range launch.Protected {
			if agent.Within(prot, w) || agent.Within(w, prot) && w != want {
				// settings/shims must not be writable; the shared login dir is
				if strings.Contains(prot, "settings") || strings.Contains(prot, "config.json") || strings.Contains(prot, agent.RolesDir) {
					if agent.Within(prot, w) || (agent.Within(w, prot) && prot != want) {
						t.Fatalf("writable %q overlaps protected %q", w, prot)
					}
				}
			}
		}
	}
	foundLogin := false
	for _, w := range settings.Filesystem.AllowWrite {
		if w == want {
			foundLogin = true
		}
	}
	if !foundLogin {
		t.Fatal("shared login dir is not writable (token refresh needs write)")
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
