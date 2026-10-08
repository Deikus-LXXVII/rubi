package relay

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
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
	busy chan struct{} // bounds concurrent answers
	pool *pool
	ctx  context.Context
}

// Hooks connects Rubi's hook addresses (/h/<address>/<id> on Rubi Gateway) to their handler.
type Hooks struct {
	Relays []string
	Route  func() string // the route secret; "" while there are no hooks (e.g. Rubi is locked)
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
	s.busy = make(chan struct{}, maxAnswers)
	var p *pool
	p = newPool(s.Relays, func(url string) []map[string]any {
		filters := []map[string]any{{"kinds": []int{Kind}, "#p": []string{s.Key.Public()}, "since": time.Now().Add(-time.Minute).Unix()}}
		if s.Announce != nil {
			filters = append(filters, s.Announce.filter())
		}
		if s.Hooks != nil && contains(s.Hooks.Relays, url) {
			if route := s.Hooks.Route(); route != "" {
				filters = append(filters, map[string]any{"kinds": []int{HookKind}, "#hs": []string{route},
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
		select { // a flood of requests is dropped rather than answered in unbounded parallel
		case s.busy <- struct{}{}:
		default:
			return
		}
		go func() {
			defer func() { <-s.busy }()
			s.answer(ctx, p, e.PubKey, pt.R, payload)
		}()
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
	addr := HookAddress(route)
	var tagged bool
	for _, t := range e.Tags {
		tagged = tagged || len(t) >= 2 && t[0] == "h" && t[1] == addr
	}
	var in struct {
		ID   string `json:"id"`
		Body string `json:"body"`
	}
	if !tagged || json.Unmarshal([]byte(e.Content), &in) != nil || !hookID(in.ID) || len(in.Body) > maxHookBody {
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
	if status == http.StatusBadRequest {
		// Not a request for this Rubi (it didn't decrypt): stay silent, as the HTTP path does for
		// anything it can't open. Answering would let anyone make Rubi publish to every relay.
		return
	}
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

const (
	maxAnswers  = 8
	maxHookBody = 16 << 10
)

// hookID accepts the ids Rubi hands out (base64url, at most 43 characters); a gateway can't push
// anything else to plugins.
func hookID(id string) bool {
	if id == "" || len(id) > 43 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// HookAddress is the route part of hook URLs, derived from the route secret. Rubi subscribes with the
// secret; the gateway derives the address to match hook events, so a URL reveals only the address.
func HookAddress(secret string) string {
	sum := sha256.Sum256([]byte("rubi-hook-route-v1\n" + secret))
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}
