// Package events collects things integrations want the agent (and through it, the user) to know,
// such as "a reply to a tracked email arrived". Delivery to the Grok Bot routine webhook is added in M4.
package events

import (
	"crypto/rand"
	"encoding/base64"
	"sort"
	"sync"
	"time"
)

type Event struct {
	ID          string         `json:"event_id"`
	Integration string         `json:"integration"`
	Type        string         `json:"type"`
	Data        map[string]any `json:"data"`
	// UntrustedFields lists dotted paths in Data that came from third parties (e.g. "reply.subject").
	UntrustedFields []string  `json:"untrusted_fields,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	Acked           bool      `json:"acked"`
	// Quiet events wait in the list for the agent's next turn instead of waking it (a wake costs quota).
	Quiet bool `json:"-"`
	// Target is the agent this event is for ("" = the default agent).
	Target string `json:"for_agent,omitempty"`
}

type Store struct {
	mu     sync.Mutex
	events map[string]*Event
	notify func(Event)
}

func NewStore() *Store { return &Store{events: map[string]*Event{}} }

// OnEmit registers a callback for new events (used for webhook delivery).
func (s *Store) OnEmit(fn func(Event)) {
	s.mu.Lock()
	s.notify = fn
	s.mu.Unlock()
}

func (s *Store) Emit(integration, typ string, data map[string]any, untrusted []string) Event {
	return s.EmitFor("", false, integration, typ, data, untrusted)
}

// EmitQuiet records an event without waking the agent.
func (s *Store) EmitQuiet(integration, typ string, data map[string]any, untrusted []string) Event {
	return s.EmitFor("", true, integration, typ, data, untrusted)
}

// EmitFor records an event for a particular agent ("" = the default one); quiet events don't wake it.
func (s *Store) EmitFor(target string, quiet bool, integration, typ string, data map[string]any, untrusted []string) Event {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	e := &Event{ID: "evt_" + base64.RawURLEncoding.EncodeToString(b), Integration: integration, Type: typ,
		Data: data, UntrustedFields: untrusted, CreatedAt: time.Now().UTC(), Quiet: quiet, Target: target}
	s.mu.Lock()
	s.events[e.ID] = e
	fn := s.notify
	s.mu.Unlock()
	if fn != nil {
		go fn(*e)
	}
	return *e
}

func (s *Store) List(includeAcked bool) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Event{}
	for _, e := range s.events {
		if includeAcked || !e.Acked {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *Store) Ack(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.events[id]; ok && !e.Acked {
		e.Acked = true
		return true
	}
	return false
}

// Clear drops everything (on lock, event data is private).
func (s *Store) Clear() {
	s.mu.Lock()
	s.events = map[string]*Event{}
	s.mu.Unlock()
}
