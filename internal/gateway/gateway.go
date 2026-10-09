// Package gateway is Rubi Gateway: a small, purpose-built Nostr relay that carries only Rubi's traffic.
//
// It is an optional transport for people who'd rather not depend on public relays. It forwards Rubi's
// ephemeral panel events (kind 21777) between a Rubi and its panel, and keeps the latest release
// announcement (kind 30078, d=rubi-releases) from the announcement key. It also accepts web requests at
// hook addresses (POST /h/<route>/<id>, e.g. from iPhone Shortcuts) and hands them to the Rubi that
// subscribed to that route (kind 21779). Nothing else is accepted and nothing is stored on disk. The panel protocol is end-to-end encrypted, so the gateway only sees ciphertext,
// sizes and timing, like any relay.
package gateway

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/watch"
)

// Limits protect the gateway from abuse.
type Limits struct {
	MaxConns        int           // concurrent connections in all
	MaxConnsPerIP   int           // concurrent connections from one address (an IPv6 /64 counts as one)
	MaxSubsPerConn  int           // subscriptions per connection
	MaxEventBytes   int64         // largest message accepted
	EventsPerMinute int           // per connection
	IdleTimeout     time.Duration // drop connections that send nothing (pings count)
}

func DefaultLimits() Limits {
	return Limits{MaxConns: 2000, MaxConnsPerIP: 64, MaxSubsPerConn: 8, MaxEventBytes: 64 << 10, EventsPerMinute: 600,
		IdleTimeout: 10 * time.Minute}
}

const (
	maxFiltersPerReq = 4
	maxFilterValues  = 8
	maxPanelContent  = 32 << 10 // one relay chunk and its envelope
	sendQueue        = 64       // messages waiting for a slow reader before it is dropped
)

type filter struct {
	Kinds   []int    `json:"kinds"`
	Authors []string `json:"authors"`
	P       []string `json:"#p"`
	D       []string `json:"#d"`
	// HS holds hook route secrets. The gateway matches hook events by the address derived from each
	// (relay.HookAddress), so knowing a hook's URL is not enough to subscribe to that Rubi's hooks.
	HS    []string `json:"#hs"`
	hooks []string
}

type conn struct {
	ws     *websocket.Conn
	out    chan []byte
	mu     sync.Mutex
	subs   map[string][]filter
	active atomic.Int64 // unix time of the last message or ping from the client
}

// Server is the gateway.
type Server struct {
	AnnounceKey string // only announcements signed by this key are kept and forwarded
	Limits      Limits
	Logf        func(string, ...any)
	// ClientIPHeader names the header that carries the client's address when requests come from a
	// local proxy (Cloudflare Tunnel: CF-Connecting-IP). Requests from loopback without it share one
	// bucket, so a proxy that doesn't set it can't be used to dodge the per-address limits.
	ClientIPHeader string
	// Watch, when set, watches Rubis that ask to be watched and wakes their administrator when one goes
	// down or stays locked (POST /watch; its key at GET /watch/key). See internal/watch.
	Watch *watch.Watcher

	mu       sync.Mutex
	conns    map[*conn]bool
	perIP    map[string]int
	announce *relay.Event // latest announcement
	hookKey  *relay.Key   // signs hook events
	hookRate map[string]*bucket
}

func New(announceKey string) *Server {
	key, err := relay.NewKey()
	if err != nil {
		panic(err)
	}
	return &Server{AnnounceKey: announceKey, Limits: DefaultLimits(), Logf: log.Printf, ClientIPHeader: "CF-Connecting-IP",
		conns: map[*conn]bool{}, perIP: map[string]int{}, hookKey: key, hookRate: map[string]*bucket{}}
}

// Handler serves WebSocket connections at any path, and a plain health check for GET without upgrade.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/h/") {
			s.serveHook(w, r)
			return
		}
		if s.Watch != nil && (r.URL.Path == "/watch" || r.URL.Path == "/watch/key") {
			s.serveWatch(w, r)
			return
		}
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("Rubi Gateway: a relay for Rubi's end-to-end encrypted panel traffic.\n"))
			return
		}
		s.serveWS(w, r)
	})
}

// clientIP is the key for per-address limits: the client's IPv4 address, or its IPv6 /64 (one
// subscriber usually has a whole /64).
func (s *Server) clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if isLoopback(r.RemoteAddr) {
		host = "unknown"
		if s.ClientIPHeader != "" {
			if v := strings.TrimSpace(r.Header.Get(s.ClientIPHeader)); v != "" {
				host = v
			}
		}
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip.String()
}

func isLoopback(addr string) bool {
	host, _, _ := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	s.mu.Lock()
	if s.perIP[ip] >= s.Limits.MaxConnsPerIP || s.Limits.MaxConns > 0 && len(s.conns) >= s.Limits.MaxConns {
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

	c := &conn{out: make(chan []byte, sendQueue), subs: map[string][]filter{}}
	c.active.Store(time.Now().UnixMilli())
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // any web origin: the panel's encryption, not CORS, protects the traffic
		// Rubi keeps idle connections open with pings; they count as activity.
		OnPingReceived: func(context.Context, []byte) bool { c.active.Store(time.Now().UnixMilli()); return true },
	})
	if err != nil {
		return
	}
	c.ws = ws
	ws.SetReadLimit(s.Limits.MaxEventBytes)
	s.mu.Lock()
	s.conns[c] = true
	s.mu.Unlock()
	ctx, stop := context.WithCancel(r.Context())
	defer func() {
		stop()
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		ws.CloseNow()
	}()
	go c.writer(ctx)
	go func() { // drop connections that stay silent (no messages, no pings)
		t := time.NewTicker(max(s.Limits.IdleTimeout/4, 10*time.Millisecond))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if time.Since(time.UnixMilli(c.active.Load())) > s.Limits.IdleTimeout {
					ws.CloseNow()
					return
				}
			}
		}
	}()

	window, count := time.Now(), 0
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		c.active.Store(time.Now().UnixMilli())
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
				c.write([]any{"NOTICE", "rate limited"})
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
	if len(msg)-2 > maxFiltersPerReq {
		c.write([]any{"CLOSED", id, "too many filters"})
		return
	}
	var fs []filter
	for _, raw := range msg[2:] {
		var f filter
		if json.Unmarshal(raw, &f) != nil || len(f.Kinds) > maxFilterValues || len(f.Authors) > maxFilterValues ||
			len(f.P) > maxFilterValues || len(f.D) > maxFilterValues || len(f.HS) > 2 {
			continue
		}
		for _, secret := range f.HS {
			f.hooks = append(f.hooks, relay.HookAddress(secret))
		}
		f.HS = nil
		fs = append(fs, f)
	}
	c.mu.Lock()
	if _, exists := c.subs[id]; !exists && len(c.subs) >= s.Limits.MaxSubsPerConn {
		c.mu.Unlock()
		c.write([]any{"CLOSED", id, "too many subscriptions"})
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
				c.write([]any{"EVENT", id, ann})
				break
			}
		}
	}
	c.write([]any{"EOSE", id})
}

func (s *Server) event(ctx context.Context, c *conn, raw json.RawMessage) {
	var e relay.Event
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	reject := func(why string) { c.write([]any{"OK", e.ID, false, "blocked: " + why}) }
	if err := relay.Verify(&e); err != nil {
		reject("invalid event")
		return
	}
	now := time.Now()
	switch e.Kind {
	case relay.Kind:
		// Exactly one recipient key and a payload the size of one chunk: the gateway carries Rubi's
		// panel traffic, not anyone's messages.
		if !onlyRecipient(&e) {
			reject("panel events must name one recipient")
			return
		}
		if len(e.Content) > maxPanelContent {
			reject("too large")
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
	c.write([]any{"OK", e.ID, true, ""})
	s.broadcast(&e)
}

// broadcast sends an event to every matching subscription and returns how many connections got it.
func (s *Server) broadcast(e *relay.Event) int {
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	n := 0
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
			c.write([]any{"EVENT", id, e})
		}
		if len(ids) > 0 {
			n++
		}
	}
	return n
}

// write queues a message for the connection. A reader too slow to keep up is disconnected rather than
// allowed to hold up everyone else's traffic.
func (c *conn) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	select {
	case c.out <- b:
	default:
		c.ws.CloseNow()
	}
}

func (c *conn) writer(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-c.out:
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.ws.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				c.ws.CloseNow()
				return
			}
		}
	}
}

func matches(f filter, e *relay.Event) bool {
	if e.Kind == relay.HookKind && !containsStr(f.hooks, tagValue(e, "h")) {
		return false // hook events go only to whoever holds their route's secret
	}
	if e.Kind == relay.Kind && len(f.P) == 0 && len(f.Authors) == 0 {
		return false // panel traffic only for its sender or recipient: no watching everyone's
	}
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

func onlyRecipient(e *relay.Event) bool {
	n := 0
	for _, t := range e.Tags {
		if len(t) >= 1 && t[0] == "p" {
			if n++; n > 1 || len(t) < 2 || !hex64(t[1]) {
				return false
			}
		}
	}
	return n == 1
}

func hex64(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

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

// ---- hooks ----

// A hook address is /h/<route>/<id>: the route says which Rubi gets it, the id which of its hooks. The
// route in the URL is derived from a secret only that Rubi holds and subscribes with (only here, never
// on public relays), so a URL holder can trigger its hook but can't listen for the others.
// The request body (at most 4 KB) goes to the Rubi as is; Rubi checks the id.

const maxHookBody = 4 << 10

type bucket struct {
	tokens float64
	last   time.Time
}

// allow is a token bucket: burst requests at once, refilled at perMinute.
func (s *Server) allow(key string, burst, perMinute float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	b := s.hookRate[key]
	if b == nil {
		if len(s.hookRate) > 10000 {
			for k, v := range s.hookRate {
				if now.Sub(v.last) > 10*time.Minute {
					delete(s.hookRate, k)
				}
			}
			if len(s.hookRate) > 10000 { // under a flood of new keys: refuse rather than grow
				return false
			}
		}
		b = &bucket{tokens: burst, last: now}
		s.hookRate[key] = b
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Minutes()*perMinute)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func validHookPart(p string) bool {
	if len(p) < 16 || len(p) > 64 {
		return false
	}
	for _, r := range p {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (s *Server) serveHook(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/h/"), "/")
	if len(parts) != 2 || !validHookPart(parts[0]) || !validHookPart(parts[1]) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(15 * time.Second)) // no trickled bodies
	if !s.allow("ip:"+s.clientIP(r), 60, 60) || !s.allow("route:"+parts[0], 30, 30) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	content, _ := json.Marshal(map[string]string{"id": parts[1], "body": string(body)})
	e := &relay.Event{CreatedAt: time.Now().Unix(), Kind: relay.HookKind, Tags: [][]string{{"h", parts[0]}}, Content: string(content)}
	if err := s.hookKey.Sign(e); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// The same answer whether or not the Rubi is connected: a URL holder learns nothing about when its
	// owner's Rubi is online or unlocked.
	s.broadcast(e)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"ok":true}` + "\n"))
}

// ---- watch ----

func (s *Server) serveWatch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/watch/key" {
		_ = json.NewEncoder(w).Encode(map[string]string{"x25519": base64.RawURLEncoding.EncodeToString(s.Watch.Key.PublicKey().Bytes())})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(15 * time.Second))
	if !s.allow("watch:"+s.clientIP(r), 10, 10) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	var m watch.Message
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&m); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.Watch.Receive(m); err != nil {
		http.Error(w, "refused", http.StatusBadRequest)
		return
	}
	_, _ = w.Write([]byte(`{"ok":true}` + "\n"))
}
