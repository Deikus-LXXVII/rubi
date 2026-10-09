// Package watch tells the user when their Rubi stops working or stays locked, from somewhere else: Rubi
// Gateway (on a separate server) or Rubi Home (on the user's computer). A Rubi that is down can't say so
// itself, and a locked Rubi can't reach anyone: the address of the user's Bot is in its closed vault.
//
// So Rubi sends each watcher, every minute, a beat signed with its watch key ("alive, unlocked" or
// "alive, locked"), and with it its registration: the administrator Bot's webhook, sealed to the watcher
// (Rubi keeps the sealed copy, so it can send it while locked without being able to read it). The watcher
// wakes the administrator when beats stop, when Rubi stays locked, and when it is back.
package watch

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/e2e"
)

// Path is the e2e path registrations are sealed for.
const Path = "/watch/v1"

var b64 = base64.RawURLEncoding

// Registration is what a watcher needs to wake the administrator; it reaches the watcher sealed.
type Registration struct {
	WatchKey string `json:"watch_key"` // Ed25519 public key that signs this Rubi's beats
	Name     string `json:"name"`      // how the notices name this Rubi (its fingerprint)
	Admin    string `json:"admin"`     // the administrator Bot's name
	URL      string `json:"url"`       // its routine webhook
	Key      string `json:"key"`       // the webhook's bearer key
}

// Beat says the Rubi is alive, and whether it is unlocked.
type Beat struct {
	ID     string `json:"id"` // b64 of the watch key
	TS     int64  `json:"ts"`
	State  string `json:"state"`            // "unlocked" or "locked"
	Reason string `json:"reason,omitempty"` // for state "stopping": why Rubi is about to stop
	Sig    string `json:"sig"`
}

// Message is what Rubi sends: a beat, and the sealed registration.
type Message struct {
	Beat         Beat        `json:"beat"`
	Registration e2e.Request `json:"registration"`
}

func (b Beat) signed() []byte {
	return []byte(fmt.Sprintf("rubi-watch-beat|v1|%s|%d|%s|%s", b.ID, b.TS, b.State, b.Reason))
}

var clock = time.Now // tests move it

// NewMessage signs a beat and attaches the sealed registration.
func NewMessage(key ed25519.PrivateKey, state, reason string, sealed e2e.Request) Message {
	b := Beat{ID: b64.EncodeToString(key.Public().(ed25519.PublicKey)), TS: clock().Unix(), State: state, Reason: reason}
	b.Sig = b64.EncodeToString(ed25519.Sign(key, b.signed()))
	return Message{Beat: b, Registration: sealed}
}

// Seal seals a registration to a watcher's X25519 public key.
func Seal(watcher *ecdh.PublicKey, r Registration) (e2e.Request, error) {
	b, _ := json.Marshal(r)
	req, _, err := e2e.Client{InstanceKey: watcher}.Seal(Path, b)
	return req, err
}

// Limits a watcher applies.
type Config struct {
	DownAfter   time.Duration // no beat for this long: "down"
	LockedAfter time.Duration // locked for this long: "locked"
	Check       time.Duration
	Max         int // Rubis watched at most
	Client      *http.Client
	Now         func() time.Time
	Logf        func(string, ...any)
	// Insecure allows http and private addresses for webhooks (tests only).
	Insecure bool
}

func DefaultConfig() Config {
	return Config{DownAfter: 5 * time.Minute, LockedAfter: 10 * time.Minute, Check: 30 * time.Second, Max: 5000,
		Client: publicOnlyClient(), Now: time.Now, Logf: func(string, ...any) {}}
}

// publicOnlyClient posts only to public internet addresses: a registration names any web address, and a
// watcher must not be steered at its own machine or network.
func publicOnlyClient() *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, _ := net.SplitHostPort(address)
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("not a public address")
		}
		return nil
	}}
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Watcher keeps the watched Rubis in memory (Rubi resends its registration with every beat, so nothing
// needs to be stored) and wakes their administrators.
type Watcher struct {
	Key *ecdh.PrivateKey
	Cfg Config

	mu    sync.Mutex
	rubis map[string]*rubi
}

type rubi struct {
	reg        Registration
	last       time.Time // last beat
	state      string
	lockedFrom time.Time
	down       bool // told "down"
	locked     bool // told "locked"
}

func New(key *ecdh.PrivateKey) *Watcher {
	return &Watcher{Key: key, Cfg: DefaultConfig(), rubis: map[string]*rubi{}}
}

// Receive checks and records one message from a Rubi.
func (w *Watcher) Receive(m Message) error {
	now := w.Cfg.Now()
	b := m.Beat
	pub, err := b64.DecodeString(b.ID)
	sig, err2 := b64.DecodeString(b.Sig)
	if err != nil || err2 != nil || len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, b.signed(), sig) {
		return errors.New("bad signature")
	}
	if d := now.Sub(time.Unix(b.TS, 0)); d > 2*time.Minute || d < -2*time.Minute {
		return errors.New("stale beat")
	}
	if b.State != "unlocked" && b.State != "locked" && b.State != "stopping" {
		return errors.New("bad state")
	}
	plain, _, err := e2e.Server{Key: w.Key}.Open(Path, m.Registration)
	if err != nil {
		return errors.New("registration not for this watcher")
	}
	var reg Registration
	if json.Unmarshal(plain, &reg) != nil || reg.WatchKey != b.ID || reg.Key == "" || len(reg.URL) > 2048 ||
		!strings.HasPrefix(reg.URL, "https://") && !(w.Cfg.Insecure && strings.HasPrefix(reg.URL, "http://")) {
		return errors.New("bad registration")
	}
	w.mu.Lock()
	r := w.rubis[b.ID]
	if r == nil {
		if len(w.rubis) >= w.Cfg.Max {
			w.mu.Unlock()
			return errors.New("too many watched")
		}
		r = &rubi{}
		w.rubis[b.ID] = r
	}
	r.reg = reg
	if !r.last.IsZero() && time.Unix(b.TS, 0).Before(r.last) {
		w.mu.Unlock()
		return nil // an older beat (replayed or late)
	}
	wasDown := r.down
	r.last, r.down = time.Unix(b.TS, 0), false
	if b.State == "locked" {
		if r.state != "locked" {
			r.lockedFrom = r.last
		}
	} else {
		r.locked = false
	}
	prev := r.state
	r.state = b.State
	w.mu.Unlock()

	switch {
	case b.State == "stopping":
		w.notify(reg, "stopping", "Rubi \""+reg.Name+"\" is stopping: "+b.Reason+". It will start again locked; when it does, it needs unlocking (rubi_status gives the link).")
	case wasDown:
		w.notify(reg, "back", "Rubi \""+reg.Name+"\" is running again ("+b.State+"). Call rubi_status: if it is locked, give the user the unlock link.")
	case prev == "locked" && b.State == "unlocked":
		// Unlocked again: Rubi itself tells its Bots what waited.
	}
	return nil
}

// Run checks the watched Rubis until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(w.Cfg.Check)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Check()
		}
	}
}

// Check sends the notices that are due.
func (w *Watcher) Check() {
	now := w.Cfg.Now()
	type due struct {
		reg      Registration
		kind, tx string
	}
	var out []due
	w.mu.Lock()
	for id, r := range w.rubis {
		if now.Sub(r.last) > 7*24*time.Hour { // gone for a week: forget it
			delete(w.rubis, id)
			continue
		}
		if !r.down && r.state != "stopping" && now.Sub(r.last) > w.Cfg.DownAfter {
			r.down = true
			out = append(out, due{r.reg, "down", fmt.Sprintf("Rubi \"%s\" stopped answering %s ago (its computer may have restarted, or Rubi crashed). Tell the user; it can't act or watch until it runs again. You'll hear when it's back.", r.reg.Name, now.Sub(r.last).Round(time.Minute))})
		}
		if !r.down && r.state == "stopping" && now.Sub(r.last) > w.Cfg.DownAfter {
			r.down = true // already told why
		}
		if r.state == "locked" && !r.locked && !r.down && now.Sub(r.lockedFrom) > w.Cfg.LockedAfter {
			r.locked = true
			out = append(out, due{r.reg, "locked", fmt.Sprintf("Rubi \"%s\" has been locked for %s, so it can't act or watch. Call rubi_status and give the user the unlock link.", r.reg.Name, now.Sub(r.lockedFrom).Round(time.Minute))})
		}
	}
	w.mu.Unlock()
	for _, d := range out {
		w.notify(d.reg, d.kind, d.tx)
	}
}

func (w *Watcher) notify(reg Registration, kind, text string) {
	body, _ := json.Marshal(map[string]any{"type": "rubi.watch." + kind, "integration": "rubi",
		"created_at": w.Cfg.Now().UTC(), "data": map[string]any{"rubi": reg.Name, "for_agent": reg.Admin}, "next_step": text})
	req, err := http.NewRequest(http.MethodPost, reg.URL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+reg.Key)
	resp, err := w.Cfg.Client.Do(req)
	if err != nil {
		w.Cfg.Logf("watch: notify failed")
		return
	}
	resp.Body.Close()
}
