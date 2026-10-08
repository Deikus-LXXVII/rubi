package relay

import (
	"context"
	"encoding/json"
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
	Logf  func(format string, args ...any)
}

// Run serves until ctx ends.
func (s *Server) Run(ctx context.Context) {
	asm := newAssembler()
	var p *pool
	p = newPool(s.Relays, func() map[string]any {
		return map[string]any{"kinds": []int{Kind}, "#p": []string{s.Key.Public()}, "since": time.Now().Add(-time.Minute).Unix()}
	}, func(e *Event) {
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
	p.run(ctx)
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
