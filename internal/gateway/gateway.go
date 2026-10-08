// Package gateway is Rubi Gateway: a small, purpose-built Nostr relay that carries only Rubi's traffic.
//
// It is an optional transport for people who'd rather not depend on public relays. It forwards Rubi's
// ephemeral panel events (kind 21777) between a Rubi and its panel, and keeps the latest release
// announcement (kind 30078, d=rubi-releases) from the announcement key. Nothing else is accepted and nothing
// is stored on disk. The panel protocol is end-to-end encrypted, so the gateway only sees ciphertext,
// sizes and timing, like any relay.
package gateway

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Deikus-LXXVII/rubi/internal/relay"
)

// Limits protect the gateway from abuse.
type Limits struct {
	MaxConnsPerIP   int           // concurrent connections from one address
	MaxSubsPerConn  int           // subscriptions per connection
	MaxEventBytes   int64         // largest message accepted
	EventsPerMinute int           // per connection
	IdleTimeout     time.Duration // drop connections that send nothing (pings count)
}

func DefaultLimits() Limits {
	return Limits{MaxConnsPerIP: 64, MaxSubsPerConn: 8, MaxEventBytes: 128 << 10, EventsPerMinute: 600,
		IdleTimeout: 10 * time.Minute}
}

type filter struct {
	Kinds   []int    `json:"kinds"`
	Authors []string `json:"authors"`
	P       []string `json:"#p"`
	D       []string `json:"#d"`
}

type conn struct {
	ws   *websocket.Conn
	wmu  sync.Mutex
	mu   sync.Mutex
	subs map[string][]filter
}

// Server is the gateway.
type Server struct {
	AnnounceKey string // only announcements signed by this key are kept and forwarded
	Limits      Limits
	Logf        func(string, ...any)

	mu       sync.Mutex
	conns    map[*conn]bool
	perIP    map[string]int
	announce *relay.Event // latest announcement
}

func New(announceKey string) *Server {
	return &Server{AnnounceKey: announceKey, Limits: DefaultLimits(), Logf: log.Printf,
		conns: map[*conn]bool{}, perIP: map[string]int{}}
}

// Handler serves WebSocket connections at any path, and a plain health check for GET without upgrade.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("Rubi Gateway: a relay for Rubi's end-to-end encrypted panel traffic.\n"))
			return
		}
		s.serveWS(w, r)
	})
}

func clientIP(r *http.Request) string {
	// Behind the local TLS proxy the real address is in X-Forwarded-For (first hop).
	if f := r.Header.Get("X-Forwarded-For"); f != "" && isLoopback(r.RemoteAddr) {
		return strings.TrimSpace(strings.Split(f, ",")[0])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func isLoopback(addr string) bool {
	host, _, _ := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	s.mu.Lock()
	if s.perIP[ip] >= s.Limits.MaxConnsPerIP {
		s.mu.Unlock()
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}
	s.perIP[ip]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.perIP[ip]--; s.perIP[ip] <= 0 {
			delete(s.perIP, ip)
		}
		s.mu.Unlock()
	}()

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true}) // any web origin: the panel's encryption, not CORS, protects the traffic
	if err != nil {
		return
	}
	ws.SetReadLimit(s.Limits.MaxEventBytes)
	c := &conn{ws: ws, subs: map[string][]filter{}}
	s.mu.Lock()
	s.conns[c] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		ws.CloseNow()
	}()

	ctx := r.Context()
	window, count := time.Now(), 0
	for {
		rctx, cancel := context.WithTimeout(ctx, s.Limits.IdleTimeout)
		_, data, err := ws.Read(rctx)
		cancel()
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
			s.req(ctx, c, msg)
		case "CLOSE":
			var id string
			_ = json.Unmarshal(msg[1], &id)
			c.mu.Lock()
			delete(c.subs, id)
			c.mu.Unlock()
		case "EVENT":
			if time.Since(window) > time.Minute {
				window, count = time.Now(), 0
			}
			if count++; count > s.Limits.EventsPerMinute {
				c.write(ctx, []any{"NOTICE", "rate limited"})
				continue
			}
			s.event(ctx, c, msg[1])
		}
	}
}

func (s *Server) req(ctx context.Context, c *conn, msg []json.RawMessage) {
	var id string
	_ = json.Unmarshal(msg[1], &id)
	if id == "" || len(id) > 64 {
		return
	}
	var fs []filter
	for _, raw := range msg[2:] {
		var f filter
		if json.Unmarshal(raw, &f) == nil {
			fs = append(fs, f)
		}
	}
	c.mu.Lock()
	if _, exists := c.subs[id]; !exists && len(c.subs) >= s.Limits.MaxSubsPerConn {
		c.mu.Unlock()
		c.write(ctx, []any{"CLOSED", id, "too many subscriptions"})
		return
	}
	c.subs[id] = fs
	c.mu.Unlock()
	s.mu.Lock()
	ann := s.announce
	s.mu.Unlock()
	if ann != nil {
		for _, f := range fs {
			if matches(f, ann) {
				c.write(ctx, []any{"EVENT", id, ann})
				break
			}
		}
	}
	c.write(ctx, []any{"EOSE", id})
}

func (s *Server) event(ctx context.Context, c *conn, raw json.RawMessage) {
	var e relay.Event
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	reject := func(why string) { c.write(ctx, []any{"OK", e.ID, false, "blocked: " + why}) }
	if err := relay.Verify(&e); err != nil {
		reject("invalid event")
		return
	}
	now := time.Now()
	switch e.Kind {
	case relay.Kind:
		if !hasTag(&e, "p") {
			reject("panel events must name a recipient")
			return
		}
		t := time.Unix(e.CreatedAt, 0)
		if t.Before(now.Add(-2*time.Minute)) || t.After(now.Add(2*time.Minute)) {
			reject("stale event")
			return
		}
	case relay.AnnounceKind:
		if e.PubKey != s.AnnounceKey || tagValue(&e, "d") != relay.AnnounceTag {
			reject("only Rubi release announcements are accepted")
			return
		}
		s.mu.Lock()
		if s.announce == nil || e.CreatedAt > s.announce.CreatedAt {
			cp := e
			s.announce = &cp
		}
		s.mu.Unlock()
	default:
		reject("this gateway only carries Rubi traffic")
		return
	}
	c.write(ctx, []any{"OK", e.ID, true, ""})
	s.broadcast(ctx, &e)
}

func (s *Server) broadcast(ctx context.Context, e *relay.Event) {
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.mu.Lock()
		var ids []string
		for id, fs := range c.subs {
			for _, f := range fs {
				if matches(f, e) {
					ids = append(ids, id)
					break
				}
			}
		}
		c.mu.Unlock()
		for _, id := range ids {
			c.write(ctx, []any{"EVENT", id, e})
		}
	}
}

func (c *conn) write(ctx context.Context, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = c.ws.Write(wctx, websocket.MessageText, b)
}

func matches(f filter, e *relay.Event) bool {
	if len(f.Kinds) > 0 && !containsInt(f.Kinds, e.Kind) {
		return false
	}
	if len(f.Authors) > 0 && !containsStr(f.Authors, e.PubKey) {
		return false
	}
	if len(f.P) > 0 && !containsStr(f.P, tagValue(e, "p")) {
		return false
	}
	if len(f.D) > 0 && !containsStr(f.D, tagValue(e, "d")) {
		return false
	}
	return true
}

func hasTag(e *relay.Event, name string) bool { return tagValue(e, name) != "" }

func tagValue(e *relay.Event, name string) string {
	for _, t := range e.Tags {
		if len(t) >= 2 && t[0] == name {
			return t[1]
		}
	}
	return ""
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
