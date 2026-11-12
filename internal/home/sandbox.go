package home

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"github.com/Deikus-LXXVII/rubi/internal/agenttask"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
)

// ResolveAgentLaunch re-reads Rubi's own configuration and committed role on every invocation.
// It does not open Helper/Manager: those constructors repair stores and load credentials.
func ResolveAgentLaunch(b agentsandbox.Binding, bindingPath string) (agentsandbox.Launch, error) {
	return resolveAgentLaunch(b, bindingPath, false)
}

// ResolveProjectServer uses the same config/active-role check, not a prompt folder.
func ResolveProjectServer(b agentsandbox.Binding, bindingPath string) (agentsandbox.Launch, error) {
	if b.Provider != "cursor" {
		return agentsandbox.Launch{}, errors.New("sandbox: unsupported project server driver")
	}
	return resolveAgentLaunch(b, bindingPath, true)
}
func resolveAgentLaunch(b agentsandbox.Binding, bindingPath string, projectServer bool) (agentsandbox.Launch, error) {
	var l agentsandbox.Launch
	if !filepath.IsAbs(b.HomeDir) || !roles.IsID(b.Role) || b.Project == "" || b.DirHash == "" {
		return l, errors.New("sandbox: missing project binding")
	}
	if _, ok := agentsandbox.Route(b.Provider); !ok && !projectServer {
		return l, fmt.Errorf("sandbox: %s: %s", b.Provider, agentsandbox.RouteBlocker(b.Provider))
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
	if b.Task != "" {
		state, err := (agenttask.Store{Home: b.HomeDir}).Get(context.Background(), b.Task)
		if err != nil || !state.Phase.Mutable() {
			return l, errors.New("sandbox: task mandate missing, sealed or ended")
		}
		w, err := agenttask.Resolve(b.HomeDir, b.Task, b.Project, b.Role, b.DirHash)
		if err != nil {
			return l, err
		}
		project = w.Dir
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
	if !ok || (!projectServer && provider.CLI == "") {
		return l, fmt.Errorf("sandbox: provider %s is not provisioned", b.Provider)
	}
	base := filepath.Join(b.HomeDir, "t3", "sandbox", b.Role, b.Provider)
	if b.Task != "" {
		base = filepath.Join(base, "tasks", b.Task)
	}
	// One server-level provider credential folder, shared by every project and
	// role (decision 4 of docs/plans/t3-integration.md). It is read-write so a
	// provider can refresh tokens; sessions in every project therefore share
	// that login. The folder is outside project trees and the supervisor's
	// settings/shims.
	login, err := agent.EnsureSignInDir(agent.SignInDirOf(b.HomeDir))
	if err != nil {
		return l, err
	}
	var credentialFiles []string
	for _, entry := range install.Providers {
		if entry.CredentialsFile != "" {
			credentialFiles = append(credentialFiles, entry.CredentialsFile)
		}
		for _, file := range entry.ProjectCredentials {
			if file != "" {
				credentialFiles = append(credentialFiles, file)
			}
		}
	}
	sensitive = append(sensitive, credentialFiles...)
	runtimeRead := append(append([]string{}, install.RuntimeRead...), provider.RuntimeRead...)
	l = agentsandbox.Launch{
		Policy: agentsandbox.Policy{Projects: []string{project}, Home: filepath.Join(base, "home"), Login: login,
			Tmp: filepath.Join(base, "tmp"), SensitiveRoots: sensitive, RuntimeRead: runtimeRead,
			AllowedDomains: provider.AllowedDomains},
		SettingsDir: filepath.Join(base, "settings"), ConfigPath: bindingPath,
		SRT: install.SRT, CLI: provider.CLI, SearchPath: install.SearchPath,
	}
	l.Protected = []string{filepath.Join(b.HomeDir, "config.json"), filepath.Join(b.HomeDir, agent.RolesDir, b.Role, "index.json"), filepath.Join(b.HomeDir, agent.RolesDir, b.Role, "versions"), filepath.Join(b.HomeDir, "t3", "bin")}
	l.OwnProcessTree = b.Task != ""
	l.Provider = b.Provider
	l.CredentialsFile = provider.CredentialsFile
	if file := provider.ProjectCredentials[b.Project]; file != "" {
		l.CredentialsFile = file
	}
	// T3 also probes instances from its server directory. Ambient cwd never selects
	// a project: preserve a workdir only inside the bound tree, otherwise use its root.
	l.Cwd = project
	if cwd, err := os.Getwd(); err == nil {
		if real, err := filepath.EvalSymlinks(cwd); err == nil && agent.Within(real, project) {
			l.Cwd = real
		}
	}
	self, err := os.Executable()
	if err != nil {
		return l, err
	}
	l, err = agentsandbox.ResolveRuntime(l, self)
	if err != nil {
		return l, err
	}
	for _, path := range l.Policy.RuntimeRead {
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
	if err := checkCredentialReadGrants(install, l.Policy.RuntimeRead); err != nil {
		return l, err
	}
	return l, nil
}

// Check both explicit metadata and additional whole-server runtime read grants.
func checkCredentialReadGrants(install agentsandbox.Installation, reads []string) error {
	for _, provider := range install.Providers {
		files := []string{provider.CredentialsFile}
		for _, file := range provider.ProjectCredentials {
			files = append(files, file)
		}
		for _, file := range files {
			if file == "" {
				continue
			}
			secret, err := filepath.EvalSymlinks(file)
			if err != nil {
				return errors.New("sandbox: configured credential file missing")
			}
			for _, path := range reads {
				real, err := filepath.EvalSymlinks(path)
				if err != nil {
					return errors.New("sandbox: runtime read path missing")
				}
				if agent.Within(real, secret) || agent.Within(secret, real) {
					return errors.New("sandbox: runtime read grant overlaps provider credentials")
				}
			}
		}
	}
	return nil
}
