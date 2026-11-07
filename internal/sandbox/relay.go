package sandbox

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Relay is a loopback Nostr relay, the same shape as the one the tests use, so the daemon and the
// fake Home can talk without leaving the sandbox network.
type Relay struct {
	URL string

	mu    sync.Mutex
	conns map[*relayClient]bool
	srv   *http.Server
}

type relayFilter struct {
	Kinds   []int    `json:"kinds"`
	Authors []string `json:"authors"`
	P       []string `json:"#p"`
}

type relayEvent struct {
	ID     string     `json:"id"`
	PubKey string     `json:"pubkey"`
	Kind   int        `json:"kind"`
	Tags   [][]string `json:"tags"`
}

type relayClient struct {
	ws   *websocket.Conn
	mu   sync.Mutex
	subs map[string][]relayFilter
}

// ListenRelay serves the relay on addr (for example 127.0.0.1:18791).
func ListenRelay(addr string) (*Relay, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	r := &Relay{URL: "ws://" + ln.Addr().String(), conns: map[*relayClient]bool{}}
	r.srv = &http.Server{Handler: http.HandlerFunc(r.serve), ReadHeaderTimeout: 10 * time.Second}
	go r.srv.Serve(ln)
	return r, nil
}

// Close stops the relay.
func (r *Relay) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return r.srv.Shutdown(ctx)
}

func (r *Relay) serve(w http.ResponseWriter, req *http.Request) {
	// The panel is on another loopback port, so its Origin is not this relay's host.
	ws, err := websocket.Accept(w, req, &websocket.AcceptOptions{OriginPatterns: []string{"127.0.0.1:*", "localhost:*"}})
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	c := &relayClient{ws: ws, subs: map[string][]relayFilter{}}
	r.mu.Lock()
	r.conns[c] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.conns, c)
		r.mu.Unlock()
		ws.CloseNow()
	}()
	ctx := req.Context()
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
			_ = json.Unmarshal(msg[1], &id)
			var fs []relayFilter
			for _, raw := range msg[2:] {
				var f relayFilter
				_ = json.Unmarshal(raw, &f)
				fs = append(fs, f)
			}
			c.mu.Lock()
			c.subs[id] = fs
			c.mu.Unlock()
			c.write(ctx, []any{"EOSE", id})
		case "CLOSE":
			var id string
			_ = json.Unmarshal(msg[1], &id)
			c.mu.Lock()
			delete(c.subs, id)
			c.mu.Unlock()
		case "EVENT":
			var e relayEvent
			_ = json.Unmarshal(msg[1], &e)
			c.write(ctx, []any{"OK", e.ID, true, ""})
			r.broadcast(ctx, e, msg[1])
		}
	}
}

func (c *relayClient) write(ctx context.Context, v any) {
	b, _ := json.Marshal(v)
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.Write(ctx, websocket.MessageText, b)
}

func (r *Relay) broadcast(ctx context.Context, e relayEvent, raw json.RawMessage) {
	r.mu.Lock()
	conns := make([]*relayClient, 0, len(r.conns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.Unlock()
	for _, c := range conns {
		c.mu.Lock()
		var ids []string
		for id, fs := range c.subs {
			for _, f := range fs {
				if relayMatch(f, e) {
					ids = append(ids, id)
					break
				}
			}
		}
		c.mu.Unlock()
		for _, id := range ids {
			c.write(ctx, []any{"EVENT", id, raw})
		}
	}
}

func relayMatch(f relayFilter, e relayEvent) bool {
	if len(f.Kinds) > 0 && !containsInt(f.Kinds, e.Kind) {
		return false
	}
	if len(f.Authors) > 0 && !containsStr(f.Authors, e.PubKey) {
		return false
	}
	if len(f.P) > 0 {
		ok := false
		for _, t := range e.Tags {
			if len(t) >= 2 && t[0] == "p" && containsStr(f.P, t[1]) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func containsInt(l []int, v int) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func containsStr(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}
