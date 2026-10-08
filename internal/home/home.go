// Package home is Rubi Home, the helper that runs on a computer at the user's home and does, on Rubi's
// request, what only a device on the home network can: talk to the Philips Hue Bridge, and run the user's
// Shortcuts (which can control Apple Home). See internal/homeproto for how Rubi reaches it.
//
// It only does what it was built for: Hue requests are limited to the bridge's resource API, and only
// shortcuts in one folder (Rubi, by default) are visible and runnable. Everything else is refused.
package home

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
	"github.com/Deikus-LXXVII/rubi/internal/identity"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

// Config is kept in config.json in the helper's directory (0600).
type Config struct {
	Name     string    `json:"name"`
	RelayKey string    `json:"relay_key"` // hex; the routing key stays fixed so Rubi can find the helper
	Relays   []string  `json:"relays,omitempty"`
	Paired   []Pairing `json:"paired,omitempty"`
	Pending  *Pending  `json:"pending,omitempty"`
	// ShortcutsFolder is the only Shortcuts folder Rubi can see and run.
	ShortcutsFolder string `json:"shortcuts_folder"`
	// HuePins maps a bridge id to the SHA-256 of its certificate, recorded when it was paired.
	HuePins map[string]string `json:"hue_pins,omitempty"`
}

// Pairing is one Rubi allowed to use the helper; only a hash of its token is kept.
type Pairing struct {
	TokenHash string    `json:"token_hash"`
	Label     string    `json:"label,omitempty"`
	At        time.Time `json:"at"`
}

type Pending struct {
	Secret  string    `json:"secret"`
	Expires time.Time `json:"expires"`
}

const pairingTTL = 15 * time.Minute

// Helper is a running Rubi Home.
type Helper struct {
	Dir string
	ID  *identity.Identity
	// Shortcuts runs the shortcuts command (a seam for tests).
	Shortcuts func(ctx context.Context, args ...string) ([]byte, error)
	Hue       *Hue

	mu  sync.Mutex
	cfg Config
}

// DefaultDir is ~/Library/Application Support/Rubi Home on a Mac (the user config dir elsewhere).
func DefaultDir() string {
	if d := os.Getenv("RUBI_HOME_DIR"); d != "" {
		return d
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, "Rubi Home")
}

// Open loads (or creates) the helper's identity and configuration.
func Open(dir string) (*Helper, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	id, err := identity.LoadOrCreate(filepath.Join(dir, "identity.json"))
	if err != nil {
		return nil, err
	}
	h := &Helper{Dir: dir, ID: id, Shortcuts: runShortcuts}
	if err := h.load(); err != nil {
		return nil, err
	}
	h.Hue = &Hue{Pins: h.huePins, SetPin: h.setHuePin}
	return h, nil
}

func (h *Helper) path() string { return filepath.Join(h.Dir, "config.json") }

func (h *Helper) load() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.loadLocked()
}

func (h *Helper) loadLocked() error {
	cfg := Config{}
	b, err := os.ReadFile(h.path())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return fmt.Errorf("config.json: %w", err)
		}
	}
	changed := false
	if cfg.RelayKey == "" {
		k, err := relay.NewKey()
		if err != nil {
			return err
		}
		cfg.RelayKey, changed = k.Hex(), true
	}
	if cfg.ShortcutsFolder == "" {
		cfg.ShortcutsFolder, changed = "Rubi", true
	}
	if cfg.Name == "" {
		n, _ := os.Hostname()
		cfg.Name, changed = n, true
	}
	h.cfg = cfg
	if changed {
		return h.saveLocked()
	}
	return nil
}

func (h *Helper) saveLocked() error {
	b, err := json.MarshalIndent(h.cfg, "", "  ")
	if err != nil {
		return err
	}
	return vault.WriteFileAtomic(h.path(), b, 0o600)
}

// Fresh returns the configuration as it is on disk now (the CLI may have changed it).
func (h *Helper) Fresh() Config {
	_ = h.load()
	return h.Config()
}

// Config returns a copy of the configuration.
func (h *Helper) Config() Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

// Update changes the configuration and saves it.
func (h *Helper) Update(fn func(*Config)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.loadLocked(); err != nil {
		return err
	}
	fn(&h.cfg)
	return h.saveLocked()
}

// RelayKey is the helper's fixed routing key.
func (h *Helper) RelayKey() (*relay.Key, error) { return relay.KeyFromHex(h.Config().RelayKey) }

// Relays the helper listens on.
func (h *Helper) Relays() []string {
	if r := h.Config().Relays; len(r) > 0 {
		return r
	}
	return homeproto.DefaultRelays()
}

// StartPairing makes a new one-time pairing code (valid 15 minutes).
func (h *Helper) StartPairing() (homeproto.Code, error) {
	secret := homeproto.RandomToken()
	if err := h.Update(func(c *Config) { c.Pending = &Pending{Secret: secret, Expires: time.Now().Add(pairingTTL)} }); err != nil {
		return homeproto.Code{}, err
	}
	key, err := h.RelayKey()
	if err != nil {
		return homeproto.Code{}, err
	}
	cfg := h.Config()
	return homeproto.Code{Relay: key.Public(), Bundle: h.ID.PublicBundle(), Relays: cfg.Relays, Secret: secret, Name: cfg.Name}, nil
}

func tokenHash(t string) string {
	sum := sha256.Sum256([]byte("rubi-home-token|" + t))
	return hex.EncodeToString(sum[:])
}

func (h *Helper) paired(token string) bool {
	if token == "" {
		return false
	}
	_ = h.load() // pick up `rubi-home unpair` run from another process
	want := tokenHash(token)
	ok := false
	for _, p := range h.Config().Paired {
		ok = ok || subtle.ConstantTimeCompare([]byte(p.TokenHash), []byte(want)) == 1
	}
	return ok
}

// Handle serves one request from Rubi.
func (h *Helper) Handle(ctx context.Context, env homeproto.Envelope) (any, error) {
	if env.Op == "pair" {
		return h.pair(env.Args)
	}
	if !h.paired(env.Token) {
		return nil, errors.New(homeproto.NotPairedMessage)
	}
	switch env.Op {
	case "hello":
		return h.hello(), nil
	case "unpair":
		want := tokenHash(env.Token)
		return nil, h.Update(func(c *Config) {
			kept := c.Paired[:0]
			for _, p := range c.Paired {
				if p.TokenHash != want {
					kept = append(kept, p)
				}
			}
			c.Paired = kept
		})
	case "shortcuts.list":
		return h.listShortcuts(ctx)
	case "shortcuts.run":
		var in struct {
			Name  string `json:"name"`
			Input string `json:"input"`
		}
		if err := json.Unmarshal(env.Args, &in); err != nil {
			return nil, errors.New("bad arguments")
		}
		return h.runShortcut(ctx, in.Name, in.Input)
	case "hue.discover":
		return h.Hue.Discover(ctx)
	case "hue.pair":
		var in struct {
			IP string `json:"ip"`
		}
		if err := json.Unmarshal(env.Args, &in); err != nil {
			return nil, errors.New("bad arguments")
		}
		return h.Hue.Pair(ctx, in.IP)
	case "hue.request":
		var in HueRequest
		if err := json.Unmarshal(env.Args, &in); err != nil {
			return nil, errors.New("bad arguments")
		}
		return h.Hue.Do(ctx, in)
	}
	return nil, fmt.Errorf("Rubi Home doesn't do %q", env.Op)
}

func (h *Helper) hello() map[string]any {
	cfg := h.Config()
	return map[string]any{"name": cfg.Name, "version": version.Version, "shortcuts_folder": cfg.ShortcutsFolder,
		"can": []string{"hue", "shortcuts"}}
}

func (h *Helper) pair(args json.RawMessage) (any, error) {
	var in struct {
		Secret string `json:"secret"`
		Token  string `json:"token"`
		Label  string `json:"label"`
	}
	if err := json.Unmarshal(args, &in); err != nil || len(in.Token) < 32 {
		return nil, errors.New("bad arguments")
	}
	var err error
	uerr := h.Update(func(c *Config) {
		p := c.Pending
		if p == nil || time.Now().After(p.Expires) || subtle.ConstantTimeCompare([]byte(p.Secret), []byte(in.Secret)) != 1 {
			err = errors.New("this pairing code has expired or was already used; run `rubi-home pair` again")
			return
		}
		c.Pending = nil
		c.Paired = append(c.Paired, Pairing{TokenHash: tokenHash(in.Token), Label: in.Label, At: time.Now().UTC()})
	})
	if uerr != nil {
		return nil, uerr
	}
	if err != nil {
		return nil, err
	}
	return h.hello(), nil
}

func (h *Helper) huePins() map[string]string { return h.Fresh().HuePins }

func (h *Helper) setHuePin(bridge, pin string) error {
	return h.Update(func(c *Config) {
		if c.HuePins == nil {
			c.HuePins = map[string]string{}
		}
		c.HuePins[bridge] = pin
	})
}
