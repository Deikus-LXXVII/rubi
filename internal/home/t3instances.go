package home

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/Deikus-LXXVII/rubi/internal/agenttask"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
	"github.com/Deikus-LXXVII/rubi/internal/home/t3instances"
)

// t3InstancesMu makes one sync of T3's instances at a time: each computes from the roles it read, and an earlier one
// retrying on the file (its write raced another) would remove what a later one added.
var t3InstancesMu sync.Mutex

// SyncT3Instances writes T3's provider instances for the roles that exist now (t3instances.SyncSandboxInstances) and returns what
// it changed. It does nothing while no T3 server is configured.
func (h *Helper) SyncT3Instances() (t3instances.SyncResult, error) {
	t3InstancesMu.Lock()
	defer t3InstancesMu.Unlock()
	c := h.Fresh()
	if c.T3Port == 0 || c.T3BaseDir == "" {
		return t3instances.SyncResult{}, nil
	}
	self, err := os.Executable()
	if err != nil {
		return t3instances.SyncResult{}, err
	}
	agentExec := c.AgentSandbox.AgentExec
	if agentExec == "" {
		agentExec = filepath.Join(filepath.Dir(self), "rubi-agent-exec")
	}
	var want []t3instances.SandboxRole
	for _, r := range h.Agent.T3Roles() {
		want = append(want, t3instances.SandboxRole{ID: r.ID, Name: r.Name, Project: r.Project, ClaudeDir: r.ClaudeDir, ProjectDir: r.ProjectDir})
	}
	tasks, err := agenttask.List(h.Dir)
	if err != nil {
		return t3instances.SyncResult{}, err
	}
	originals := append([]t3instances.SandboxRole(nil), want...)
	for _, w := range tasks {
		for _, r := range originals {
			if r.ID != w.Role || r.Project != w.Project {
				continue
			}
			resolved, err := agenttask.Resolve(h.Dir, w.ID, r.Project, r.ID, roles.DirHash(r.ProjectDir))
			if err != nil {
				continue
			}
			r.Task, r.Workspace = w.ID, resolved.Dir
			want = append(want, r)
		}
	}
	return t3instances.SyncSandboxInstances(t3instances.InstanceOptions{T3Base: c.T3BaseDir, Dir: h.Dir, Projects: h.T3.syncProjects()}, agentExec, want)
}
