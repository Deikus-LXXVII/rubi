// Package events collects things integrations want the agent (and through it, the user) to know,
// such as "a reply to a tracked email arrived". Delivery to the Grok Bot routine webhook is added in M4.
package events

import (
	"crypto/rand"
	"encoding/base64"
	"sort"
	"strings"
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
	// Acked is whether the agent reading the event has acknowledged it. The store fills it per reader
	// (ListFor); in saved state it is only the old, global flag, read back as "acked by everyone".
	Acked bool `json:"acked"`
	// AckedBy records when each agent (lower-case name) acknowledged the event; AckedAll ("*") means all
	// agents did. It is kept in saved state and never shown to a Bot (ListFor strips it).
	AckedBy map[string]time.Time `json:"acked_by,omitempty"`
	// Quiet events wait in the list for the agent's next turn instead of waking it (a wake costs quota).
	Quiet bool `json:"-"`
	// Target is the only agent this event is for ("" = the subscribers, or the default agent).
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
	s.pruneLocked(e.CreatedAt)
	s.events[e.ID] = e
	fn := s.notify
	s.mu.Unlock()
	if fn != nil {
		go fn(*e)
	}
	return *e
}

// AckedAll is the AckedBy key meaning "every agent": events acked before acks were per agent, and
// events every addressee has acked.
const AckedAll = "*"

// AgentKey is how agent names are compared in AckedBy: like the rest of Rubi, case-insensitive.
func AgentKey(agent string) string { return strings.ToLower(strings.TrimSpace(agent)) }

func (e *Event) ackedBy(key string) bool {
	if _, ok := e.AckedBy[AckedAll]; ok {
		return true
	}
	_, ok := e.AckedBy[key]
	return ok
}

func (e Event) clone() Event {
	if e.AckedBy != nil {
		m := make(map[string]time.Time, len(e.AckedBy))
		for k, v := range e.AckedBy {
			m[k] = v
		}
		e.AckedBy = m
	}
	return e
}

// List returns copies of the events, newest first, with their full state (for saving). Without
// includeAcked, events acked by everyone are left out.
func (s *Store) List(includeAcked bool) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Event{}
	for _, e := range s.events {
		_, all := e.AckedBy[AckedAll]
		if includeAcked || !all {
			c := e.clone()
			c.Acked = all
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// ListFor is the event list as agent sees it: Acked tells whether this agent acked the event, and an
// event acked by agent X stays in the list for agent Y until Y acks it. Other agents' acks are not shown.
func (s *Store) ListFor(agent string, includeAcked bool) []Event {
	key := AgentKey(agent)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Event{}
	for _, e := range s.events {
		acked := e.ackedBy(key)
		if includeAcked || !acked {
			c := *e
			c.AckedBy, c.Acked = nil, acked
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Get returns a copy of one event as stored (no per-agent view).
func (s *Store) Get(id string) (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.events[id]; ok {
		return e.clone(), true
	}
	return Event{}, false
}

// AckResult says what an acknowledgement did.
type AckResult string

const (
	Acked        AckResult = "acked"         // this agent's first ack
	AlreadyAcked AckResult = "already_acked" // this agent acked it before
	NotFound     AckResult = "unknown_or_expired"
)

// Ack records that agent acknowledged the event. Whether the agent may see it is the caller's business.
func (s *Store) Ack(agent, id string) AckResult {
	key := AgentKey(agent)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[id]
	if !ok {
		return NotFound
	}
	if e.ackedBy(key) {
		return AlreadyAcked
	}
	if e.AckedBy == nil {
		e.AckedBy = map[string]time.Time{}
	}
	e.AckedBy[key] = time.Now().UTC()
	return Acked
}

// AckAll marks the event as acked by every agent (used once every addressee acked it, so it can be
// pruned early).
func (s *Store) AckAll(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.events[id]; ok {
		if e.AckedBy == nil {
			e.AckedBy = map[string]time.Time{}
		}
		if _, done := e.AckedBy[AckedAll]; !done {
			e.AckedBy[AckedAll] = time.Now().UTC()
		}
	}
}

// Delivered-but-never-stored events carry ids with these prefixes (plus 8 random characters); a stored id
// is "evt_" and 12 characters. They can't be acked.
var notStoredPrefixes = []string{"evt_access_", "evt_code_", "evt_locked_", "evt_stopping_"}

// NotStored reports whether id belongs to an event that is only delivered to a webhook, never kept.
func NotStored(id string) bool {
	if len(id) == len("evt_")+12 {
		return false
	}
	for _, p := range notStoredPrefixes {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}

// Restore puts back events saved before Rubi was locked or restarted (ids are kept, nothing is delivered).
// Events saved with the old global Acked flag count as acked by everyone.
func (s *Store) Restore(list []Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range list {
		e := list[i].clone()
		if _, ok := s.events[e.ID]; !ok && e.ID != "" {
			if e.Acked {
				if e.AckedBy == nil {
					e.AckedBy = map[string]time.Time{}
				}
				if _, done := e.AckedBy[AckedAll]; !done {
					e.AckedBy[AckedAll] = time.Now().UTC()
				}
			}
			e.Acked = false
			s.events[e.ID] = &e
		}
	}
}

// Clear drops everything (on lock, event data is private).
func (s *Store) Clear() {
	s.mu.Lock()
	s.events = map[string]*Event{}
	s.mu.Unlock()
}

// Events are private and only useful for a while: ones acknowledged by everyone go after an hour, others
// (with their per-agent acks) after a day, and the list never holds more than maxEvents (the oldest go first).
const maxEvents = 1000

func (s *Store) pruneLocked(now time.Time) {
	for id, e := range s.events {
		_, all := e.AckedBy[AckedAll]
		if all && now.Sub(e.CreatedAt) > time.Hour || now.Sub(e.CreatedAt) > 24*time.Hour {
			delete(s.events, id)
		}
	}
	for len(s.events) >= maxEvents {
		var oldest *Event
		for _, e := range s.events {
			if oldest == nil || e.CreatedAt.Before(oldest.CreatedAt) {
				oldest = e
			}
		}
		delete(s.events, oldest.ID)
	}
}
