package t3vm

// One VM per role on a backend whose VMs Home runs itself (docs/plans/t3-cloud-agents.md slice 3b): a role is pinned to
// one project, and every streamed session of the role runs in the role's VM, which has the project's root and T3's
// attachments folder (the only folders a session may add, ClampArgv) besides the role's own. The VM's proxy, hook gate
// and T3 forward serve all its sessions; the gate files asks under the role and names each session's T3 thread from the
// claude session id Home started it with. The VM stops some time after its last session ends, when its pipe keeps
// failing, or when Home stops serving.

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// roleVMIdle is how long a role's VM stays up after its last session (a var for tests).
var roleVMIdle = 10 * time.Minute

// roleVM is a role's running VM and what serves it.
type roleVM struct {
	role string
	vm   agent.SessionVM
	// proxyEnv is what every session's environment gets for the VM's proxy.
	proxyEnv map[string]string
	// ctx ends when the VM is stopped; its sessions end with it.
	ctx    context.Context
	cancel context.CancelFunc

	ready chan struct{} // closed when the start is over (err says how it went)
	err   error

	mu       sync.Mutex
	refs     int
	idle     *time.Timer
	stopped  bool
	threads  map[string]string // claude session id → T3 thread
	teardown []func()          // last first
}

// threadOf is the gate's ThreadOf.
func (v *roleVM) threadOf(sessionID string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.threads[sessionID]
}

// stop ends the VM and what serves it, once.
func (v *roleVM) stop(logf func(string, ...any)) {
	v.mu.Lock()
	if v.stopped {
		v.mu.Unlock()
		return
	}
	v.stopped = true
	if v.idle != nil {
		v.idle.Stop()
	}
	steps := v.teardown
	v.teardown = nil
	v.mu.Unlock()
	v.cancel()
	for i := len(steps) - 1; i >= 0; i-- {
		steps[i]()
	}
	_ = logf
}

// vmPool holds the role VMs of a Server.
type vmPool struct {
	mu  sync.Mutex
	vms map[string]*roleVM
}

// acquire returns the role's running VM with one more session on it, starting it with start when there is none (one
// start at a time per role; others wait for it). release ends that session's hold: the last one starts the idle timer.
func (p *vmPool) acquire(ctx context.Context, role string, start func(v *roleVM) error, logf func(string, ...any)) (*roleVM, func(), error) {
	for {
		p.mu.Lock()
		if p.vms == nil {
			p.vms = map[string]*roleVM{}
		}
		v, ok := p.vms[role]
		if !ok {
			v = &roleVM{role: role, ready: make(chan struct{}), threads: map[string]string{}}
			v.ctx, v.cancel = context.WithCancel(context.Background())
			p.vms[role] = v
			p.mu.Unlock()
			v.err = start(v)
			if v.err != nil {
				p.drop(v)
				v.stop(logf)
			}
			close(v.ready)
		} else {
			p.mu.Unlock()
		}
		select {
		case <-v.ready:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		if v.err != nil {
			return nil, nil, v.err
		}
		v.mu.Lock()
		if v.stopped { // stopped meanwhile (idle, pipe failure): start a new one
			v.mu.Unlock()
			p.drop(v)
			continue
		}
		v.refs++
		if v.idle != nil {
			v.idle.Stop()
			v.idle = nil
		}
		v.mu.Unlock()
		var once sync.Once
		release := func() {
			once.Do(func() {
				v.mu.Lock()
				defer v.mu.Unlock()
				v.refs--
				if v.refs == 0 && !v.stopped {
					v.idle = time.AfterFunc(roleVMIdle, func() {
						v.mu.Lock()
						busy := v.refs > 0
						v.mu.Unlock()
						if !busy {
							p.drop(v)
							v.stop(logf)
						}
					})
				}
			})
		}
		return v, release, nil
	}
}

// drop forgets v (when it is still the role's entry).
func (p *vmPool) drop(v *roleVM) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.vms[v.role] == v {
		delete(p.vms, v.role)
	}
}

// stopAll ends every role VM (Home stops serving).
func (p *vmPool) stopAll(logf func(string, ...any)) {
	p.mu.Lock()
	vms := make([]*roleVM, 0, len(p.vms))
	for _, v := range p.vms {
		vms = append(vms, v)
	}
	p.vms = map[string]*roleVM{}
	p.mu.Unlock()
	for _, v := range vms {
		<-v.ready
		v.stop(logf)
	}
}

// startRoleVM boots the role's VM with spec's folders and starts its proxy, hook gate, T3 forward and pipe.
func (s *Server) startRoleVM(ctx context.Context, v *roleVM, role *agent.T3Role, spec SessionSpec, mounts []agent.Mount) error {
	o := s.opt
	sctx, cancel := context.WithTimeout(ctx, startLimit)
	defer cancel()
	vm, err := o.Stream.StartSessionVM(sctx, spec.Name, mounts)
	if err != nil {
		return fmt.Errorf("starting the VM: %w", err)
	}
	v.vm = vm
	add := func(f func()) { v.mu.Lock(); v.teardown = append(v.teardown, f); v.mu.Unlock() }
	add(func() {
		rctx, c := context.WithTimeout(context.Background(), stopLimit)
		defer c()
		if err := vm.Stop(rctx); err != nil {
			o.Logf("t3vm: stopping %s: %v", spec.Name, err)
		}
	})
	proxyL, hookL, t3L := netpipe.NewListener("proxy", 16), netpipe.NewListener("hook", 16), netpipe.NewListener("t3", 16)
	px, proxyToken, err := o.Backend.T3Proxy(role, proxyL)
	if err != nil {
		return err
	}
	add(func() { px.Close() })
	g := NewGate(o.Backend, Target{Project: role.Project, Role: role.ID, Cwd: role.ProjectDir})
	g.Logf, g.ThreadOf = o.Logf, v.threadOf
	hook := &http.Server{Handler: g, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = hook.Serve(hookL) }()
	add(func() { hook.Close(); hookL.Close() })
	fwd := &forwarder{dial: o.DialT3}
	fwd.wg.Add(1)
	go fwd.serve(t3L)
	add(func() { t3L.Close(); fwd.close() })
	stopPipe, err := vm.StartPipe(sctx, &agent.PipeSpec{Proxy: proxyL, Hook: hookL, T3: t3L}, spec.PipeEnv(), func() {
		o.Logf("t3vm: the network pipe of %s keeps failing; ending its sessions", spec.Name)
		s.pool.drop(v)
		go v.stop(o.Logf)
	})
	if err != nil {
		return fmt.Errorf("the network pipe: %w", err)
	}
	add(stopPipe)
	v.proxyEnv = map[string]string{"HTTPS_PROXY": "http://rubi:" + proxyToken + "@" + agent.PipeProxyAddr, "NO_PROXY": "127.0.0.1"}
	return nil
}
