package relay

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Server answers panel requests that arrive through relays.
type Server struct {
	Key    *Key
	Relays []string
	// Handle processes one encrypted panel request and returns the encrypted response.
	Handle func(ctx context.Context, body []byte) (status int, resp []byte)
	// Ready reports how many relays are connected (0 = the transport is down).
	Ready func(connected int)
	// Announce, when set, also follows release announcements (see announce.go).
	Announce *Announcements
	// Hooks, when set, receives web requests at this Rubi's hook addresses through the relays in
	// Hooks.Relays (Rubi Gateway only: the route must not reach public relays).
	Hooks *Hooks
	Logf  func(format string, args ...any)

	mu   sync.Mutex
	pool *pool
	ctx  context.Context
}

// Hooks connects Rubi's hook addresses (/h/<route>/<id> on Rubi Gateway) to their handler.
type Hooks struct {
	Relays []string
	Route  func() string // "" while there are none (e.g. Rubi is locked)
	On     func(id string, body []byte)
}

// Resubscribe applies a changed hook route on the open connections.
func (s *Server) Resubscribe() {
	s.mu.Lock()
	p, ctx := s.pool, s.ctx
	s.mu.Unlock()
	if p != nil {
		go p.resubscribe(ctx)
	}
}

// Run serves until ctx ends.
func (s *Server) Run(ctx context.Context) {
	asm := newAssembler()
	var p *pool
	p = newPool(s.Relays, func(url string) []map[string]any {
		filters := []map[string]any{{"kinds": []int{Kind}, "#p": []string{s.Key.Public()}, "since": time.Now().Add(-time.Minute).Unix()}}
		if s.Announce != nil {
			filters = append(filters, s.Announce.filter())
		}
		if s.Hooks != nil && contains(s.Hooks.Relays, url) {
			if route := s.Hooks.Route(); route != "" {
				filters = append(filters, map[string]any{"kinds": []int{HookKind}, "#h": []string{route},
					"since": time.Now().Add(-time.Minute).Unix()})
			}
		}
		return filters
	}, func(e *Event) {
		if e.Kind == HookKind {
			s.hook(e)
			return
		}
		if e.Kind == AnnounceKind {
			if s.Announce != nil {
				s.Announce.handle(e)
			}
			return
		}
		var pt part
		if json.Unmarshal([]byte(e.Content), &pt) != nil {
			return
		}
		payload, _, done := asm.add(e.PubKey, pt)
		if !done {
			return
		}
		go s.answer(ctx, p, e.PubKey, pt.R, payload)
	}, s.Logf)
	p.ready = s.Ready
	s.mu.Lock()
	s.pool, s.ctx = p, ctx
	s.mu.Unlock()
	p.run(ctx)
}

func (s *Server) hook(e *Event) {
	if s.Hooks == nil {
		return
	}
	route := s.Hooks.Route()
	if route == "" {
		return
	}
	var tagged bool
	for _, t := range e.Tags {
		tagged = tagged || len(t) >= 2 && t[0] == "h" && t[1] == route
	}
	var in struct {
		ID   string `json:"id"`
		Body string `json:"body"`
	}
	if !tagged || json.Unmarshal([]byte(e.Content), &in) != nil || in.ID == "" {
		return
	}
	s.Hooks.On(in.ID, []byte(in.Body))
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Server) answer(ctx context.Context, p *pool, to, msgID, payload string) {
	hctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	status, resp := s.Handle(hctx, []byte(payload))
	parts, err := split(msgID, status, string(resp))
	if err != nil {
		parts, _ = split(msgID, 500, "")
	}
	events, err := signParts(s.Key, to, parts)
	if err != nil {
		return
	}
	if err := p.publish(ctx, events); err != nil && s.Logf != nil {
		s.Logf("relay answer: %v", err)
	}
}
