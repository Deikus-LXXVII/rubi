// Package core ties Rubi together: identity, vault, lock state, approvals, events, and panel links.
package core

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/audit"
	"github.com/Deikus-LXXVII/rubi/internal/events"
	"github.com/Deikus-LXXVII/rubi/internal/identity"
	"github.com/Deikus-LXXVII/rubi/internal/integrations"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

type State string

const (
	Unpaired State = "unpaired"
	Locked   State = "locked"
	Unlocked State = "unlocked"
)

// DefaultPanelOrigin is where the official static panel is served.
const DefaultPanelOrigin = "https://rubi-panel.com"

const pairingTTL = 15 * time.Minute

var ErrNoTransport = errors.New("the panel connection is not available yet (Rubi is still opening its tunnel); " +
	"try again in a few seconds")

type Core struct {
	Layout      paths.Layout
	ID          *identity.Identity
	Vault       *vault.Store
	Approvals   *approvals.Engine
	Events      *events.Store
	Audit       *audit.Log
	PanelOrigin string

	mu       sync.Mutex
	paired   bool
	endpoint string // current public URL of the panel API (set by the transport)
	pairCode string
	pairExp  time.Time
}

func Open(layout paths.Layout) (*Core, error) {
	if err := layout.Ensure(); err != nil {
		return nil, err
	}
	id, err := identity.LoadOrCreate(layout.Identity())
	if err != nil {
		return nil, err
	}
	c := &Core{
		Layout:      layout,
		ID:          id,
		Vault:       vault.NewStore(layout.Vault(), id.InstanceID),
		Events:      events.NewStore(),
		Audit:       audit.Open(layout.Audit()),
		PanelOrigin: DefaultPanelOrigin,
	}
	if o := os.Getenv("RUBI_PANEL_ORIGIN"); o != "" {
		c.PanelOrigin = strings.TrimRight(o, "/")
	}
	_, keysErr := os.Stat(layout.Keys())
	c.paired = keysErr == nil && c.Vault.Exists()
	c.Approvals = approvals.New(approvals.DefaultConfig(), approvals.Hooks{
		Level: c.PolicyLevel,
		Link:  func(id string) (string, error) { return c.Link("approve:" + id) },
		Label: c.Label,
		Audit: func(ev string, f map[string]any) { c.Audit.Record(ev, f) },
	})
	return c, nil
}

func (c *Core) State() State {
	c.mu.Lock()
	paired := c.paired
	c.mu.Unlock()
	switch {
	case !paired:
		return Unpaired
	case c.Vault.Unlocked():
		return Unlocked
	default:
		return Locked
	}
}

// SetEndpoint is called by the transport whenever the public panel-API URL changes.
func (c *Core) SetEndpoint(u string) {
	c.mu.Lock()
	c.endpoint = u
	c.mu.Unlock()
}

// Link builds a panel URL. Purposes: "pair", "unlock", "settings", "setup:<integration>", "approve:<id>".
// Everything after '#' stays in the user's browser and never reaches the panel's web server.
func (c *Core) Link(purpose string) (string, error) {
	if err := validPurpose(purpose); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.endpoint == "" {
		return "", ErrNoTransport
	}
	if purpose == "pair" && c.paired {
		return "", errors.New("Rubi is already paired; use purpose \"unlock\" or \"settings\"")
	}
	if purpose != "pair" && !c.paired {
		return "", errors.New("Rubi is not paired yet; use purpose \"pair\" first")
	}
	q := url.Values{}
	q.Set("v", "1")
	q.Set("e", c.endpoint)
	q.Set("k", c.ID.PublicBundle())
	q.Set("a", purpose)
	if purpose == "pair" {
		if c.pairCode == "" || time.Now().After(c.pairExp) {
			b := make([]byte, 16)
			_, _ = rand.Read(b)
			c.pairCode, c.pairExp = base64.RawURLEncoding.EncodeToString(b), time.Now().Add(pairingTTL)
		}
		q.Set("p", c.pairCode)
	}
	return c.PanelOrigin + "/#" + q.Encode(), nil
}

func validPurpose(p string) error {
	switch {
	case p == "pair", p == "unlock", p == "settings":
		return nil
	case strings.HasPrefix(p, "setup:"):
		if _, ok := integrations.Get(strings.TrimPrefix(p, "setup:")); ok {
			return nil
		}
		return errors.New("unknown integration in purpose " + p)
	case strings.HasPrefix(p, "approve:"):
		return nil
	}
	return errors.New(`purpose must be "pair", "unlock", "settings" or "setup:<integration id>"`)
}

// Lock wipes keys and private data from memory and cancels pending approvals. Always allowed.
func (c *Core) Lock() {
	c.Approvals.CancelAll()
	c.Events.Clear()
	c.Vault.Lock()
	c.Audit.SetVaultKey(nil)
	c.Audit.Record("rubi.locked", nil)
}

// Unlock opens the vault with a DEK delivered by the panel.
func (c *Core) Unlock(dek []byte, minVersion uint64, receipt map[string]any) error {
	if err := c.Vault.Unlock(dek, minVersion); err != nil {
		c.Audit.Record("rubi.unlock_failed", nil)
		return err
	}
	c.Audit.SetVaultKey(dek)
	c.Audit.Record("rubi.unlocked", receipt)
	return nil
}

// Pair creates the vault on first run. Called by the panel API after the pairing code is verified.
func (c *Core) Pair(dek []byte, keys *vault.Keys, data *vault.Data) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paired {
		return errors.New("already paired")
	}
	keys.Instance = c.ID.InstanceID
	if err := vault.SaveKeys(c.Layout.Keys(), keys); err != nil {
		return err
	}
	if err := c.Vault.Create(dek, data); err != nil {
		return err
	}
	c.paired, c.pairCode = true, ""
	c.Audit.SetVaultKey(dek)
	c.Audit.Record("rubi.paired", nil)
	return nil
}

// CheckPairingCode reports whether code is the current, unexpired pairing code. A successful Pair burns it.
func (c *Core) CheckPairingCode(code string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pairCode != "" && time.Now().Before(c.pairExp) &&
		subtle.ConstantTimeCompare([]byte(code), []byte(c.pairCode)) == 1
}

// PolicyLevel resolves the approval level for an action kind: locked manifest levels win, then the
// user's policy from the vault, then the manifest default. Unknown kinds are strong.
func (c *Core) PolicyLevel(kind string) approvals.Level {
	def, locked, ok := integrations.DefaultLevel(kind)
	if !ok {
		return approvals.Strong
	}
	if locked {
		return def
	}
	level := def
	_ = c.Vault.View(func(d *vault.Data) error {
		if l, ok := approvals.ParseLevel(d.Policy[kind]); ok {
			level = l
		}
		return nil
	})
	return level
}

// Label localizes button texts. English only for now; the user's locale lives in the vault.
func (c *Core) Label(s string) string { return s }
