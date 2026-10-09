package core

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/audit"
	"github.com/Deikus-LXXVII/rubi/internal/e2e"
	"github.com/Deikus-LXXVII/rubi/internal/events"
	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/internal/watch"
)

// Notify when Rubi is down or locked (see internal/watch). If the user turns it on, Rubi sends watchers
// (Rubi Gateway, the user's Rubi Home computers) a signed beat every minute, with the administrator Bot's
// webhook sealed to each watcher. The sealed copies and the watch key live outside the vault, in
// watch.json, so a locked or freshly restarted Rubi can keep beating; it can't read the webhook in them.
//
// Before Rubi locks or stops on its own, it also tells the administrator why (a beat "stopping" to the
// watchers too). That can't cover a computer that is switched off at once; the watchers do.

const beatEvery = time.Minute

type watchFile struct {
	Key     string        `json:"key"` // Ed25519 seed: signs beats only
	Targets []watchTarget `json:"targets,omitempty"`
}

type watchTarget struct {
	Kind         string      `json:"kind"` // "gateway" or "home"
	URL          string      `json:"url,omitempty"`
	Relay        string      `json:"relay,omitempty"`
	Bundle       string      `json:"bundle,omitempty"`
	Relays       []string    `json:"relays,omitempty"`
	Name         string      `json:"name,omitempty"`
	Registration e2e.Request `json:"registration"`
}

var watchMu sync.Mutex

// loadWatch reads watch.json; with create, it makes the watch key when there is none yet.
func (c *Core) loadWatch(create bool) (*watchFile, ed25519.PrivateKey) {
	watchMu.Lock()
	defer watchMu.Unlock()
	var f watchFile
	if b, err := os.ReadFile(c.Layout.Watch()); err == nil {
		_ = json.Unmarshal(b, &f)
	}
	seed, err := base64.RawURLEncoding.DecodeString(f.Key)
	if (err != nil || len(seed) != ed25519.SeedSize) && !create {
		return &f, nil
	}
	if err != nil || len(seed) != ed25519.SeedSize {
		seed = make([]byte, ed25519.SeedSize)
		_, _ = rand.Read(seed)
		f.Key = base64.RawURLEncoding.EncodeToString(seed)
		b, _ := json.MarshalIndent(f, "", "  ")
		_ = vault.WriteFileAtomic(c.Layout.Watch(), b, 0o600)
	}
	return &f, ed25519.NewKeyFromSeed(seed)
}

func (c *Core) saveWatchTargets(targets []watchTarget) {
	if _, err := os.Stat(c.Layout.Watch()); err != nil && len(targets) == 0 {
		return // never turned on: nothing to clear
	}
	f, _ := c.loadWatch(true)
	watchMu.Lock()
	defer watchMu.Unlock()
	f.Targets = targets
	b, _ := json.MarshalIndent(f, "", "  ")
	_ = vault.WriteFileAtomic(c.Layout.Watch(), b, 0o600)
}

// WatchSettings is what the user chose: which watchers tell the administrator when Rubi is down or locked.
type WatchSettings struct {
	Gateway bool `json:"gateway"`
	Home    bool `json:"home"`
}

func (c *Core) WatchSettings() WatchSettings {
	var w WatchSettings
	_ = c.Vault.View(func(d *vault.Data) error {
		if d.Watch != nil {
			w = WatchSettings{Gateway: d.Watch.Gateway, Home: d.Watch.Home}
		}
		return nil
	})
	return w
}

// SetWatch asks the user to approve which watchers to use.
func (c *Core) SetWatch(ctx context.Context, w WatchSettings) (string, error) {
	var on []string
	if w.Gateway {
		on = append(on, "Rubi Gateway")
	}
	if w.Home {
		on = append(on, "your Rubi Home computers")
	}
	summary, effect := "Stop watching whether Rubi is running", "Nobody tells your Bot when Rubi is down or locked."
	if len(on) > 0 {
		summary = "Tell your Bot when Rubi is down or locked, through " + strings.Join(on, " and ")
		effect = "Every minute Rubi tells them it is alive. If it stops, or stays locked, they wake your administrator Bot."
	}
	preview := map[string]any{"effect": effect}
	if w.Gateway {
		preview["note"] = "Rubi Gateway gets your administrator Bot's webhook (sealed so only the gateway can read it), to be able to wake it."
	}
	return c.RequestChange(ctx, summary, preview, func(d *vault.Data) error {
		d.Watch = &vault.Watch{Gateway: w.Gateway, Home: w.Home}
		return nil
	}, func() { go c.refreshWatch() })
}

// refreshWatch seals the current registration for each chosen watcher (only while unlocked: it needs the
// administrator's webhook).
func (c *Core) refreshWatch() {
	if c.State() != Unlocked {
		return
	}
	w := c.WatchSettings()
	var admin *vault.Agent
	var devices []vault.Device
	_ = c.Vault.View(func(d *vault.Data) error {
		if a := defaultAgent(d); a != nil {
			cp := *a
			admin = &cp
		}
		for _, x := range d.Devices {
			devices = append(devices, *x)
		}
		return nil
	})
	if admin == nil || (!w.Gateway && !w.Home) {
		c.saveWatchTargets(nil)
		return
	}
	_, key := c.loadWatch(true)
	reg := watch.Registration{WatchKey: base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)),
		Name: c.ID.Fingerprint(), Admin: admin.Name, URL: admin.URL, Key: admin.Key}
	var targets []watchTarget
	if w.Gateway && c.HookBase != nil {
		if base := strings.TrimRight(c.HookBase(), "/"); strings.HasPrefix(base, "https://") || strings.HasPrefix(base, "http://127.0.0.1:") {
			if pub, err := gatewayWatchKey(base); err == nil {
				if sealed, err := watch.Seal(pub, reg); err == nil {
					targets = append(targets, watchTarget{Kind: "gateway", URL: base + "/watch", Registration: sealed})
				}
			} else {
				log.Printf("watch: %v", err)
			}
		}
	}
	if w.Home {
		for _, d := range devices {
			pub, err := e2e.ParseInstanceBundle(d.Bundle)
			if err != nil {
				continue
			}
			if sealed, err := watch.Seal(pub, reg); err == nil {
				targets = append(targets, watchTarget{Kind: "home", Relay: d.Relay, Bundle: d.Bundle, Relays: d.Relays,
					Name: d.Name, Registration: sealed})
			}
		}
	}
	c.saveWatchTargets(targets)
}

func gatewayWatchKey(base string) (*ecdh.PublicKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/watch/key", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the gateway's watch key: %w", err)
	}
	defer resp.Body.Close()
	var k struct {
		X25519 string `json:"x25519"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&k) != nil {
		return nil, errors.New("the gateway doesn't watch Rubis (update it)")
	}
	b, err := base64.RawURLEncoding.DecodeString(k.X25519)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPublicKey(b)
}

// RunWatch sends beats until ctx ends.
func (c *Core) RunWatch(ctx context.Context) {
	t := time.NewTicker(beatEvery)
	defer t.Stop()
	refreshed := time.Time{}
	for {
		if c.State() == Unlocked && time.Since(refreshed) > 10*time.Minute {
			c.refreshWatch() // picks up a new administrator or webhook
			refreshed = time.Now()
		}
		state := "locked"
		if c.State() == Unlocked {
			state = "unlocked"
		}
		c.beat(ctx, state, "")
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Core) beat(ctx context.Context, state, reason string) {
	f, key := c.loadWatch(false)
	if len(f.Targets) == 0 || key == nil || c.State() == Unpaired {
		return
	}
	var wg sync.WaitGroup
	for _, t := range f.Targets {
		m := watch.NewMessage(key, state, reason, t.Registration)
		wg.Add(1)
		go func() {
			defer wg.Done()
			bctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			switch t.Kind {
			case "gateway":
				body, _ := json.Marshal(m)
				req, _ := http.NewRequestWithContext(bctx, http.MethodPost, t.URL, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				if resp, err := http.DefaultClient.Do(req); err == nil {
					resp.Body.Close()
				}
			case "home":
				cl, err := homeproto.NewClient(t.Relay, t.Bundle, t.Relays, "")
				if err != nil {
					return
				}
				defer cl.Close()
				_ = cl.Call(bctx, "watch.beat", m, nil)
			}
		}()
	}
	wg.Wait()
}

// Stopping tells the administrator, and the watchers, that Rubi is about to stop and why. It waits a few
// seconds at most.
func (c *Core) Stopping(reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); c.beat(ctx, "stopping", reason) }()
		go func() { defer wg.Done(); c.tellAdmin(ctx, "stopping", reason) }()
		wg.Wait()
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// tellAdmin wakes the administrator directly (once, no retries): used just before Rubi locks or stops,
// when there is no later chance.
func (c *Core) tellAdmin(ctx context.Context, kind, reason string) {
	if c.State() != Unlocked {
		return
	}
	var admin *vault.Agent
	_ = c.Vault.View(func(d *vault.Data) error {
		if a := defaultAgent(d); a != nil {
			cp := *a
			admin = &cp
		}
		return nil
	})
	if admin == nil {
		return
	}
	text := map[string]string{
		"locked":   "Rubi was locked: " + reason + ". Until the user unlocks it, it can't act or watch; anything that comes in meanwhile waits. Call rubi_status for the unlock link if the user wants it running.",
		"stopping": "Rubi is stopping: " + reason + ". It will start again locked; then call rubi_status and give the user the unlock link.",
	}[kind]
	ev := events.Event{ID: "evt_" + kind + "_" + randomID()[:8], Integration: "rubi", Type: kind, CreatedAt: time.Now().UTC(),
		Data: map[string]any{"reason": reason, "next_step": text}}
	body, _ := json.Marshal(map[string]any{"type": "rubi." + kind, "event_id": ev.ID, "integration": "rubi",
		"created_at": ev.CreatedAt, "data": ev.Data, "next_step": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, admin.URL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if admin.Key != "" {
		req.Header.Set("Authorization", "Bearer "+admin.Key)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	}
	c.Audit.Record("rubi."+kind+"_notice", audit.Fields{"reason": reason})
}
