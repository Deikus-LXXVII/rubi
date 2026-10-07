// Package integrations defines how integrations describe themselves to the core.
//
// In v1 integrations are compiled into the binary and register themselves from init(). The manifest is
// what the panel shows the user: which secrets are needed, which actions exist and how they are gated,
// which events can be emitted, and which hosts the integration connects to.
package integrations

import (
	"sort"
	"sync"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
)

type Secret struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Help    string `json:"help,omitempty"`
	HelpURL string `json:"help_url,omitempty"`
}

type Action struct {
	Kind         string          `json:"kind"`
	Title        string          `json:"title"`
	DefaultLevel approvals.Level `json:"default_level"`
	// Locked actions keep their level regardless of user policy (e.g. changing security settings).
	Locked  bool               `json:"locked,omitempty"`
	Options []approvals.Option `json:"options,omitempty"`
}

type EventType struct {
	Type      string   `json:"type"`
	Untrusted []string `json:"untrusted_fields,omitempty"`
}

type Manifest struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Version     string      `json:"version"`
	Description string      `json:"description"`
	Needs       string      `json:"needs"` // one sentence for the agent to tell the user before setup
	Fields      []Field     `json:"fields,omitempty"`
	Secrets     []Secret    `json:"secrets"`
	Actions     []Action    `json:"actions"`
	Events      []EventType `json:"events,omitempty"`
	Egress      []string    `json:"egress"`
}

// Integration is the minimal contract in M1. Runtime hooks (start/stop, tools) arrive with M4.
type Integration interface {
	Manifest() Manifest
}

var (
	mu       sync.RWMutex
	registry = map[string]Integration{}
)

func Register(i Integration) {
	mu.Lock()
	defer mu.Unlock()
	id := i.Manifest().ID
	if _, dup := registry[id]; dup {
		panic("integration registered twice: " + id)
	}
	registry[id] = i
}

func All() []Integration {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Integration, 0, len(registry))
	for _, i := range registry {
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Manifest().ID < out[b].Manifest().ID })
	return out
}

func Get(id string) (Integration, bool) {
	mu.RLock()
	defer mu.RUnlock()
	i, ok := registry[id]
	return i, ok
}

// DefaultLevel finds the manifest default for an action kind; ok is false for unknown kinds.
func DefaultLevel(kind string) (level approvals.Level, locked, ok bool) {
	for _, i := range All() {
		for _, a := range i.Manifest().Actions {
			if a.Kind == kind {
				return a.DefaultLevel, a.Locked, true
			}
		}
	}
	return "", false, false
}
