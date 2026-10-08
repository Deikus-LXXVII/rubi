// Package relaytest runs a minimal in-memory Nostr relay for tests: subscriptions with kinds, authors and
// #p filters, and live forwarding of events (nothing is stored, like ephemeral events on real relays).
package relaytest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

type filter struct {
	Kinds   []int    `json:"kinds"`
	Authors []string `json:"authors"`
	P       []string `json:"#p"`
}

type event struct {
	ID     string     `json:"id"`
	PubKey string     `json:"pubkey"`
	Kind   int        `json:"kind"`
	Tags   [][]string `json:"tags"`
}

type client struct {
	ws   *websocket.Conn
	mu   sync.Mutex
	subs map[string]filter
}

type Relay struct {
	Server *httptest.Server
	mu     sync.Mutex
	conns  map[*client]bool
	// Drop, when set, makes the relay accept events but forward nothing (a broken relay).
	Drop bool
}

func New() *Relay {
	r := &Relay{conns: map[*client]bool{}}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	return r
}

// URL is the relay's ws:// address.
func (r *Relay) URL() string { return "ws" + strings.TrimPrefix(r.Server.URL, "http") }

func (r *Relay) Close() { r.Server.Close() }

func (r *Relay) serve(w http.ResponseWriter, req *http.Request) {
	ws, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	c := &client{ws: ws, subs: map[string]filter{}}
	r.mu.Lock()
	r.conns[c] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.conns, c)
		r.mu.Unlock()
		ws.CloseNow()
	}()
	ctx := context.Background()
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var msg []json.RawMessage
		if json.Unmarshal(data, &msg) != nil || len(msg) < 2 {
			continue
		}
		var typ string
		_ = json.Unmarshal(msg[0], &typ)
		switch typ {
		case "REQ":
			var id string
			var f filter
			_ = json.Unmarshal(msg[1], &id)
			if len(msg) > 2 {
				_ = json.Unmarshal(msg[2], &f)
			}
			c.mu.Lock()
			c.subs[id] = f
			c.mu.Unlock()
			c.write(ctx, []any{"EOSE", id})
		case "CLOSE":
			var id string
			_ = json.Unmarshal(msg[1], &id)
			c.mu.Lock()
			delete(c.subs, id)
			c.mu.Unlock()
		case "EVENT":
			var e event
			_ = json.Unmarshal(msg[1], &e)
			c.write(ctx, []any{"OK", e.ID, true, ""})
			if !r.Drop {
				r.broadcast(ctx, e, msg[1])
			}
		}
	}
}

func (c *client) write(ctx context.Context, v any) {
	b, _ := json.Marshal(v)
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.Write(ctx, websocket.MessageText, b)
}

func (r *Relay) broadcast(ctx context.Context, e event, raw json.RawMessage) {
	r.mu.Lock()
	conns := make([]*client, 0, len(r.conns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.Unlock()
	for _, c := range conns {
		c.mu.Lock()
		var ids []string
		for id, f := range c.subs {
			if matches(f, e) {
				ids = append(ids, id)
			}
		}
		c.mu.Unlock()
		for _, id := range ids {
			c.write(ctx, []any{"EVENT", id, raw})
		}
	}
}

func matches(f filter, e event) bool {
	if len(f.Kinds) > 0 && !contains(f.Kinds, e.Kind) {
		return false
	}
	if len(f.Authors) > 0 && !containsS(f.Authors, e.PubKey) {
		return false
	}
	if len(f.P) > 0 {
		ok := false
		for _, t := range e.Tags {
			if len(t) >= 2 && t[0] == "p" && containsS(f.P, t[1]) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func contains(l []int, v int) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func containsS(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}
