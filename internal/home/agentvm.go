package home

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/t3vm"
)

// AgentVMSocketPath is the T3 session socket in dir (`rubi-home agent-vm` talks to it).
func AgentVMSocketPath(dir string) string { return t3vm.SocketPath(dir) }

// t3InstancesMu makes one sync of T3's instances at a time: each computes from the roles it read, and an earlier one
// retrying on the file (its write raced another) would remove what a later one added.
var t3InstancesMu sync.Mutex

// SyncT3Instances writes T3's provider instances for the roles that exist now (t3vm.SyncInstances) and returns what
// it changed. It does nothing while no T3 server is configured.
func (h *Helper) SyncT3Instances() (t3vm.SyncResult, error) {
	t3InstancesMu.Lock()
	defer t3InstancesMu.Unlock()
	c := h.Fresh()
	if c.T3Port == 0 || c.T3BaseDir == "" {
		return t3vm.SyncResult{}, nil
	}
	self, err := os.Executable()
	if err != nil {
		return t3vm.SyncResult{}, err
	}
	var want []t3vm.RoleInstance
	for _, r := range h.Agent.T3Roles() {
		want = append(want, t3vm.RoleInstance{ID: r.ID, Name: r.Name, ClaudeDir: r.ClaudeDir, ProjectDir: r.ProjectDir})
	}
	return t3vm.SyncInstances(t3vm.InstanceOptions{T3Base: c.T3BaseDir, Dir: h.Dir, RubiHome: self, Projects: h.T3.syncProjects()}, want)
}

// ServeAgentVM serves the T3 session socket until ctx ends (T3Home.Run starts it with the T3 server it runs; for a server
// someone else runs, Run serves it when the configuration names one). It returns at once, serving nothing, while no T3 server
// is configured (T3Port is 0).
func (h *Helper) ServeAgentVM(ctx context.Context) error {
	c := h.Fresh()
	if c.T3Port == 0 || c.T3BaseDir == "" {
		return nil
	}
	if res, err := h.SyncT3Instances(); err != nil {
		log.Printf("T3 role instances: %v", err)
	} else if res.Changed() {
		log.Printf("T3 role instances: added %v, updated %v, removed %v", res.Added, res.Updated, res.Removed)
	}
	be := h.backend()
	if av := be.Available(ctx); !av.OK {
		log.Printf("T3 sessions: not available on this computer (%s)", av.Reason)
		return nil
	}
	host := be.Sessions()
	if host == nil {
		log.Printf("T3 sessions: not available on this computer (%s)", noSessionHost)
		return nil
	}
	srv := t3vm.NewServer(t3vm.Options{Dir: h.Dir, Backend: h.Agent, Host: host, T3Port: c.T3Port, T3Pid: h.T3Pid,
		Attachments: filepath.Join(c.T3BaseDir, "userdata", "attachments")})
	h.sessMu.Lock()
	h.sess = srv
	h.sessMu.Unlock()
	defer func() {
		h.sessMu.Lock()
		if h.sess == srv {
			h.sess = nil
		}
		h.sessMu.Unlock()
	}()
	return srv.Serve(ctx, AgentVMSocketPath(h.Dir))
}

// sessionServer is the T3 session server while it serves (nil otherwise).
func (h *Helper) sessionServer() *t3vm.Server {
	h.sessMu.Lock()
	defer h.sessMu.Unlock()
	return h.sess
}
