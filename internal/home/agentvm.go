package home

import (
	"context"
	"log"
	"os"
	"path/filepath"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/t3vm"
)

// AgentVMSocketPath is the T3 session socket in dir (`rubi-home agent-vm` talks to it).
func AgentVMSocketPath(dir string) string { return t3vm.SocketPath(dir) }

// SyncT3Instances writes T3's provider instances for the roles that exist now (t3vm.SyncInstances) and returns what
// it changed. It does nothing while no T3 server is configured.
func (h *Helper) SyncT3Instances() (t3vm.SyncResult, error) {
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
		want = append(want, t3vm.RoleInstance{ID: r.ID, Name: r.Name, ClaudeDir: r.ClaudeDir})
	}
	return t3vm.SyncInstances(t3vm.InstanceOptions{T3Base: c.T3BaseDir, Dir: h.Dir, RubiHome: self}, want)
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
	agentCfg := h.Fresh()
	ac := &agent.AppleContainer{RunnerDir: agentCfg.AgentRunnerDir, EnvDir: h.Dir}
	srv := t3vm.NewServer(t3vm.Options{Dir: h.Dir, Backend: h.Agent, Host: ac, T3Port: c.T3Port, T3Pid: h.T3Pid,
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
