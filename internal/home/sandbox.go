package home

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
)

// ResolveAgentLaunch re-reads Rubi's own configuration and committed role on every invocation.
// It does not open Helper/Manager: those constructors repair stores and load credentials.
func ResolveAgentLaunch(b agentsandbox.Binding, bindingPath string) (agentsandbox.Launch, error) {
	var l agentsandbox.Launch
	if !filepath.IsAbs(b.HomeDir) || !roles.IsID(b.Role) || b.Project == "" || b.DirHash == "" {
		return l, errors.New("sandbox: missing project binding")
	}
	if _, ok := agentsandbox.Route(b.Provider); !ok {
		return l, fmt.Errorf("sandbox: provider %s has no supported sandbox launch hook", b.Provider)
	}
	data, err := os.ReadFile(filepath.Join(b.HomeDir, "config.json"))
	if err != nil {
		return l, errors.New("sandbox: cannot read Rubi configuration")
	}
	// Decode only non-secret installation/project fields. Never carry the rest into the launch.
	var c struct {
		AgentProjects []string                  `json:"agent_projects"`
		AgentSandbox  agentsandbox.Installation `json:"agent_sandbox"`
		T3BaseDir     string                    `json:"t3_base_dir"`
	}
	if json.Unmarshal(data, &c) != nil {
		return l, errors.New("sandbox: invalid Rubi configuration")
	}
	r, err := roles.ReadCurrent(filepath.Join(b.HomeDir, agent.RolesDir), b.Role)
	if err != nil || r.Status != "active" || r.Project != b.Project || r.DirHash != b.DirHash {
		return l, errors.New("sandbox: role no longer belongs to the bound project")
	}
	var project string
	projects := agent.ConfiguredProjects(b.HomeDir, []string{c.T3BaseDir}, c.AgentProjects)
	for _, p := range projects {
		if p.ID == b.Project && roles.DirHash(p.Dir) == b.DirHash {
			project = p.Dir
		}
	}
	if project == "" {
		return l, errors.New("sandbox: project missing or directory binding changed")
	}
	// Nested configured projects cannot be separated by a broad read/write grant.
	for _, p := range projects {
		if p.Dir != project && (agent.Within(p.Dir, project) || agent.Within(project, p.Dir)) {
			return l, errors.New("sandbox: overlapping configured projects cannot be isolated")
		}
	}
	sensitive := []string{b.HomeDir}
	if c.T3BaseDir != "" {
		sensitive = append(sensitive, c.T3BaseDir)
	}
	if hostHome, err := os.UserHomeDir(); err == nil {
		sensitive = append(sensitive, hostHome)
	}
	for _, p := range projects {
		sensitive = append(sensitive, p.Dir)
	}
	install := c.AgentSandbox
	provider, ok := install.Providers[b.Provider]
	if !ok || provider.CLI == "" {
		return l, fmt.Errorf("sandbox: provider %s is not provisioned", b.Provider)
	}
	base := filepath.Join(b.HomeDir, "t3", "sandbox", b.Role, b.Provider)
	login := filepath.Join(base, "login")
	if b.Provider == "claudeAgent" {
		login = filepath.Join(b.HomeDir, agent.RolesDir, b.Role, "claude")
	}
	runtimeRead := append(append([]string{}, install.RuntimeRead...), provider.RuntimeRead...)
	for _, path := range runtimeRead {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return l, errors.New("sandbox: runtime read path missing")
		}
		for _, root := range []string{filepath.Join(b.HomeDir, agent.RolesDir), filepath.Join(b.HomeDir, "config.json"), filepath.Join(b.HomeDir, "t3", "sandbox")} {
			if agent.Within(real, root) || agent.Within(root, real) {
				return l, errors.New("sandbox: runtime read grant overlaps private Rubi state")
			}
		}
		for _, p := range projects {
			if agent.Within(p.Dir, real) || agent.Within(real, p.Dir) {
				return l, errors.New("sandbox: runtime read grant overlaps a configured project")
			}
		}
	}
	l = agentsandbox.Launch{
		Policy: agentsandbox.Policy{Projects: []string{project}, Home: filepath.Join(base, "home"), Login: login,
			Tmp: filepath.Join(base, "tmp"), SensitiveRoots: sensitive, RuntimeRead: runtimeRead,
			AllowedDomains: provider.AllowedDomains},
		SettingsDir: filepath.Join(base, "settings"), ConfigPath: bindingPath,
		SRT: install.SRT, CLI: provider.CLI, SearchPath: install.SearchPath,
	}
	l.Protected = []string{filepath.Join(b.HomeDir, "config.json"), filepath.Join(b.HomeDir, agent.RolesDir, b.Role, "index.json"), filepath.Join(b.HomeDir, agent.RolesDir, b.Role, "versions"), filepath.Join(b.HomeDir, "t3", "bin")}
	l.Provider = b.Provider
	// T3 also probes instances from its server directory. Ambient cwd never selects
	// a project: preserve a workdir only inside the bound tree, otherwise use its root.
	l.Cwd = project
	if cwd, err := os.Getwd(); err == nil {
		if real, err := filepath.EvalSymlinks(cwd); err == nil && agent.Within(real, project) {
			l.Cwd = real
		}
	}
	return l, nil
}
