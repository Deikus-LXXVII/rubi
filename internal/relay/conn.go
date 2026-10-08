package relay

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// DefaultRelays are public relays checked to forward ephemeral events of Kind promptly, including
// payloads of tens of kilobytes. Any one of them is enough; Rubi and the panel use all at once.
var DefaultRelays = []string{
	"wss://nos.lol",
	"wss://relay.primal.net",
	"wss://relay.snort.social",
	"wss://nostr.mom",
	"wss://relay.nostr.net",
	"wss://nostr.oxtr.dev",
}

// chunkSize keeps every event well below common relay limits.
const (
	chunkSize   = 30000
	maxParts    = 64
	maxMessage  = chunkSize * maxParts
	readLimit   = 1 << 20
	dialTimeout = 10 * time.Second
)

// part is the content of one event: a chunk of a message.
type part struct {
	V      int    `json:"v"`
	R      string `json:"r"`           // message id (a response repeats its request's id)
	I      int    `json:"i"`           // chunk index
	N      int    `json:"n"`           // chunk count
	Status int    `json:"s,omitempty"` // responses: HTTP-like status
	D      string `json:"d"`
}

func split(msgID string, status int, payload string) ([]part, error) {
	if len(payload) > maxMessage {
		return nil, errors.New("message too large for the relay transport")
	}
	n := max(1, (len(payload)+chunkSize-1)/chunkSize)
	parts := make([]part, n)
	for i := range n {
		end := min(len(payload), (i+1)*chunkSize)
		parts[i] = part{V: 1, R: msgID, I: i, N: n, Status: status, D: payload[i*chunkSize : end]}
	}
	return parts, nil
}

// assembler joins chunks per sender and message id.
type assembler struct {
	mu      sync.Mutex
	pending map[string]*pendingMsg
}

type pendingMsg struct {
	parts   []string
	have    int
	status  int
	started time.Time
}

func newAssembler() *assembler { return &assembler{pending: map[string]*pendingMsg{}} }

// add returns the whole payload once every chunk of a message arrived.
func (a *assembler) add(sender string, p part) (payload string, status int, done bool) {
	if p.V != 1 || p.N < 1 || p.N > maxParts || p.I < 0 || p.I >= p.N || p.R == "" || len(p.R) > 64 {
		return "", 0, false
	}
	key := sender + "|" + p.R
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, m := range a.pending {
		if now.Sub(m.started) > 2*time.Minute {
			delete(a.pending, k)
		}
	}
	m := a.pending[key]
	if m == nil {
		if len(a.pending) > 200 {
			return "", 0, false
		}
		m = &pendingMsg{parts: make([]string, p.N), started: now}
		a.pending[key] = m
	}
	if len(m.parts) != p.N || m.parts[p.I] != "" {
		return "", 0, false
	}
	m.parts[p.I] = p.D
	m.have++
	if p.Status != 0 {
		m.status = p.Status
	}
	if m.have < p.N {
		return "", 0, false
	}
	delete(a.pending, key)
	return strings.Join(m.parts, ""), m.status, true
}

// relayConn is one connection to one relay with a single subscription.
type relayConn struct {
	url string
	ws  *websocket.Conn
	wmu sync.Mutex
}

func (c *relayConn) send(ctx context.Context, msg []any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.ws.Write(wctx, websocket.MessageText, b)
}

// pool keeps connections to several relays, each subscribed with the same filter, and reconnects.
type pool struct {
	relays  []string
	filter  func() map[string]any
	onEvent func(*Event)
	logf    func(string, ...any)
	ready   func(n int)

	mu    sync.Mutex
	conns map[string]*relayConn
	seen  map[string]time.Time
}

func newPool(relays []string, filter func() map[string]any, onEvent func(*Event), logf func(string, ...any)) *pool {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &pool{relays: relays, filter: filter, onEvent: onEvent, logf: logf,
		conns: map[string]*relayConn{}, seen: map[string]time.Time{}}
}

func (p *pool) run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, url := range p.relays {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.keep(ctx, url)
		}()
	}
	wg.Wait()
}

func (p *pool) connected() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

func (p *pool) setConn(url string, c *relayConn) {
	p.mu.Lock()
	if c == nil {
		delete(p.conns, url)
	} else {
		p.conns[url] = c
	}
	n := len(p.conns)
	p.mu.Unlock()
	if p.ready != nil {
		p.ready(n)
	}
}

// keep holds one relay connection open, reconnecting with backoff.
func (p *pool) keep(ctx context.Context, url string) {
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := p.session(ctx, url)
		p.setConn(url, nil)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 2*time.Minute {
			backoff = 2 * time.Second
		}
		p.logf("relay %s: %v; reconnecting in %s", url, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
}

func (p *pool) session(ctx context.Context, url string) error {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	ws, _, err := websocket.Dial(dctx, url, nil)
	cancel()
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(readLimit)
	c := &relayConn{url: url, ws: ws}
	if err := c.send(ctx, []any{"REQ", "rubi", p.filter()}); err != nil {
		return err
	}
	p.setConn(url, c)

	sctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { // keepalive: relays and middleboxes drop idle connections
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(sctx, 15*time.Second)
				err := ws.Ping(pctx)
				cancel()
				if err != nil {
					ws.CloseNow()
					return
				}
			}
		}
	}()
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		var msg []json.RawMessage
		if json.Unmarshal(data, &msg) != nil || len(msg) < 2 {
			continue
		}
		var typ string
		_ = json.Unmarshal(msg[0], &typ)
		switch typ {
		case "EVENT":
			if len(msg) < 3 {
				continue
			}
			var e Event
			if json.Unmarshal(msg[2], &e) != nil || e.Kind != Kind || !fresh(&e, time.Now()) || p.dup(e.ID) {
				continue
			}
			if Verify(&e) != nil {
				continue
			}
			p.onEvent(&e)
		case "CLOSED":
			return errors.New("subscription closed by relay: " + string(data))
		}
	}
}

// dup reports whether an event was already handled (every relay delivers its own copy).
func (p *pool) dup(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if _, ok := p.seen[id]; ok {
		return true
	}
	if len(p.seen) > 5000 {
		for k, t := range p.seen {
			if now.Sub(t) > 5*time.Minute {
				delete(p.seen, k)
			}
		}
	}
	p.seen[id] = now
	return false
}

// publish sends signed events to every connected relay; it succeeds if at least one relay took them.
func (p *pool) publish(ctx context.Context, events []*Event) error {
	p.mu.Lock()
	conns := make([]*relayConn, 0, len(p.conns))
	for _, c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	if len(conns) == 0 {
		return errors.New("no relay connected")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for _, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, e := range events {
				if c.send(ctx, []any{"EVENT", e}) != nil {
					return
				}
			}
			mu.Lock()
			ok++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if ok == 0 {
		return errors.New("no relay accepted the message")
	}
	return nil
}

func signParts(key *Key, to string, parts []part) ([]*Event, error) {
	now := time.Now().Unix()
	out := make([]*Event, 0, len(parts))
	for _, pt := range parts {
		b, err := json.Marshal(pt)
		if err != nil {
			return nil, err
		}
		e := &Event{CreatedAt: now, Kind: Kind, Tags: [][]string{{"p", to}}, Content: string(b)}
		if err := key.Sign(e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
