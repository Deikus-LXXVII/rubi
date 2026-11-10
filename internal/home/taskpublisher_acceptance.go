//go:build rubi_acceptance

// Test-only wiring. This file and local publication transports are absent from
// release builds. No config/environment field can select a publication target.
package home

import (
	"context"
	"github.com/Deikus-LXXVII/rubi/internal/agenttask"
	"github.com/Deikus-LXXVII/rubi/internal/t3"
	"sync"
)

type acceptanceDestination struct{ remote, api string }

var acceptanceDestinations sync.Map

func AcceptancePublication(h *Helper, remote, api string) {
	acceptanceDestinations.Store(h.Dir, acceptanceDestination{remote, api})
}
func taskPublisher(store agenttask.Store, helper string) agenttask.Publisher {
	if v, ok := acceptanceDestinations.Load(store.Home); ok {
		d := v.(acceptanceDestination)
		return agenttask.AcceptancePublisher(store, helper, d.remote, d.api)
	}
	return agenttask.Publisher{Store: store, Helper: helper}
}

// The server is independently paired over its real native API; attachment is
// test-only bootstrap, not an alternative production authentication mechanism.
func AcceptanceNative(h *Helper, srv *t3.Server, cl *t3.Client, ctx context.Context) {
	h.T3.mu.Lock()
	h.T3.base = ctx
	h.T3.srv = srv
	h.T3.runCtx, h.T3.runStop = context.WithCancel(ctx)
	h.T3.mu.Unlock()
	h.T3.setClient(cl)
	h.T3.resumeWatchers()
	h.T3.recoverTasks(ctx)
}

// AcceptanceCrash disconnects only this disposable daemon's watchers. It leaves
// the independently supervised native server/task alive, as a daemon crash does.
func AcceptanceCrash(h *Helper) {
	h.T3.mu.Lock()
	stop, cl := h.T3.runStop, h.T3.cl
	h.T3.runStop, h.T3.runCtx, h.T3.srv, h.T3.cl = nil, nil, nil, nil
	h.T3.watchPaused = true
	h.T3.mu.Unlock()
	if stop != nil {
		stop()
	}
	h.T3.watchWG.Wait()
	if cl != nil {
		_ = cl.Close()
	}
	h.Agent.Close()
}
