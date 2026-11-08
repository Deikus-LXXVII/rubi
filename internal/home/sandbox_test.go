package home

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/t3vm"
)

func sandboxFixture(t *testing.T) (agentsandbox.Binding, Config, string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "rubi")
	a := filepath.Join(root, "project-a")
	b := filepath.Join(root, "project-b")
	for _, p := range []string{dir, a, b} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	binding := agentsandbox.Binding{HomeDir: dir, Role: "r_aaaaaa01", Project: "project-a", DirHash: roles.DirHash(a), Provider: "claudeAgent"}
	c := Config{AgentProjects: []string{a, b}, AgentSandbox: agentsandbox.Installation{AgentExec: "/usr/bin/true", SRT: "/usr/bin/true", SearchPath: []string{"/usr/bin"}, Providers: map[string]agentsandbox.Provider{"claudeAgent": {CLI: "/usr/bin/true"}, "codex": {CLI: "/usr/bin/true"}}}}
	writeSandboxJSON(t, filepath.Join(dir, "config.json"), c)
	r := roles.Role{ID: binding.Role, Project: binding.Project, DirHash: binding.DirHash, Status: "active", Version: 1}
	rd := filepath.Join(dir, agent.RolesDir, r.ID)
	writeSandboxJSON(t, filepath.Join(rd, "versions", "1.json"), r)
	writeSandboxJSON(t, filepath.Join(rd, "index.json"), r)
	_, err = t3vm.SyncSandboxInstances(t3vm.InstanceOptions{T3Base: filepath.Join(root, "t3"), Dir: dir, Projects: []t3vm.T3Project{{ID: "ta", Root: a}, {ID: "tb", Root: b}}}, "/usr/bin/true", []t3vm.SandboxRole{{ID: r.ID, Name: "Test", Project: r.Project, ProjectDir: a, ClaudeDir: filepath.Join(rd, "claude")}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "t3", "sandbox", r.ID, "claudeAgent", "binding.json")
	return binding, c, path, b
}
func writeSandboxJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxBindingNeverUsesAnotherProjectsCwdOrEnvironment(t *testing.T) {
	b, c, path, other := sandboxFixture(t)
	t.Chdir(other)
	t.Setenv("RUBI_HOME_DIR", other)
	t.Setenv("CLAUDE_CONFIG_DIR", other)
	t.Setenv("CODEX_HOME", other)
	l, err := ResolveAgentLaunch(b, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Policy.Projects) != 1 || l.Policy.Projects[0] != c.AgentProjects[0] || l.Cwd != c.AgentProjects[0] {
		t.Fatalf("wrong project: %+v", l.Policy.Projects)
	}
	settings, err := agentsandbox.Build(l.Policy, l.Protected)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range settings.Filesystem.AllowWrite {
		if w == other || agent.Within(other, w) {
			t.Fatal("project B granted write access")
		}
	}
	found := false
	for _, d := range settings.Filesystem.DenyRead {
		if d == other {
			found = true
		}
	}
	if !found {
		t.Fatal("other project not hidden")
	}
	// Runtime and the policy/shim tree are never writable, even though login state is.
	for _, protected := range append(l.Protected, path, l.SettingsDir) {
		for _, w := range settings.Filesystem.AllowWrite {
			if agent.Within(protected, w) {
				t.Fatal("agent can write shim/policy/config")
			}
		}
	}
}

func TestSandboxBindingRefusesMissingReassignedArchivedAndUnsupported(t *testing.T) {
	for _, name := range []string{"missing", "reassigned", "archived", "wrong binding", "unsupported", "unprovisioned", "overlap", "runtime project read", "runtime role read"} {
		t.Run(name, func(t *testing.T) {
			b, c, path, other := sandboxFixture(t)
			switch name {
			case "missing":
				c.AgentProjects = nil
			case "reassigned":
				c.AgentProjects = []string{other}
			case "archived":
				r := roles.Role{ID: b.Role, Project: b.Project, DirHash: b.DirHash, Status: "archived", Version: 1}
				writeSandboxJSON(t, filepath.Join(b.HomeDir, agent.RolesDir, b.Role, "versions", "1.json"), r)
			case "wrong binding":
				b.Project = "project-b"
			case "unsupported":
				b.Provider = "cursor"
			case "unprovisioned":
				b.Provider = "pi"
			case "runtime role read":
				c.AgentSandbox.RuntimeRead = []string{filepath.Join(b.HomeDir, agent.RolesDir)}
			case "runtime project read":
				c.AgentSandbox.RuntimeRead = []string{other}
			case "overlap":
				sub := filepath.Join(c.AgentProjects[0], "nested")
				os.Mkdir(sub, 0700)
				c.AgentProjects = append(c.AgentProjects, sub)
			}
			writeSandboxJSON(t, filepath.Join(b.HomeDir, "config.json"), c)
			if _, err := ResolveAgentLaunch(b, path); err == nil {
				t.Fatal("unsafe launch accepted")
			}
		})
	}
}

func TestSandboxBindingRechecksConfigEveryLaunch(t *testing.T) {
	b, c, path, _ := sandboxFixture(t)
	if _, err := ResolveAgentLaunch(b, path); err != nil {
		t.Fatal(err)
	}
	c.AgentProjects = nil
	writeSandboxJSON(t, filepath.Join(b.HomeDir, "config.json"), c)
	if _, err := ResolveAgentLaunch(b, path); err == nil || !strings.Contains(err.Error(), "project") {
		t.Fatalf("stale binding accepted: %v", err)
	}
}

func TestSandboxRejectsWritableShimTree(t *testing.T) {
	b, _, path, _ := sandboxFixture(t)
	l, err := ResolveAgentLaunch(b, path)
	if err != nil {
		t.Fatal(err)
	}
	l.Policy.Login = filepath.Join(b.HomeDir, "t3")
	if _, err := agentsandbox.Build(l.Policy, l.Protected); err == nil {
		t.Fatal("writable shim tree accepted")
	}
}

func TestSandboxBindingCanonicalizesSymlinkedTemporaryRoot(t *testing.T) {
	b, c, path, _ := sandboxFixture(t)
	root := filepath.Dir(b.HomeDir)
	alias := filepath.Join(t.TempDir(), "var")
	if err := os.Symlink(root, alias); err != nil {
		t.Skip(err)
	}
	canonical := c.AgentProjects[0]
	c.AgentProjects = []string{filepath.Join(alias, "project-a"), filepath.Join(alias, "project-b")}
	b.HomeDir = filepath.Join(alias, "rubi")
	b.DirHash = roles.DirHash(c.AgentProjects[0])
	r := roles.Role{ID: b.Role, Project: b.Project, DirHash: b.DirHash, Status: "active", Version: 1}
	writeSandboxJSON(t, filepath.Join(b.HomeDir, agent.RolesDir, b.Role, "index.json"), r)
	writeSandboxJSON(t, filepath.Join(b.HomeDir, agent.RolesDir, b.Role, "versions", "1.json"), r)
	writeSandboxJSON(t, filepath.Join(b.HomeDir, "config.json"), c)
	l, err := ResolveAgentLaunch(b, path)
	if err != nil {
		t.Fatal("symlink-root binding failed", err)
	}
	real, err := filepath.EvalSymlinks(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Policy.Projects) != 1 || l.Policy.Projects[0] != real || b.DirHash != roles.DirHash(real) {
		t.Fatal("binding and project resolution disagree")
	}
	if _, err := agentsandbox.Build(l.Policy, l.Protected); err != nil {
		t.Fatal(err)
	}
}
