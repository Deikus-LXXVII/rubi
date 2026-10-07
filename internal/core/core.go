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

const (
	pairingTTL = 15 * time.Minute
	ticketTTL  = 15 * time.Minute
)

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
	transErr string // why there is no endpoint, if the transport can't start
	pairCode string
	pairExp  time.Time
	// tickets are short-lived random tokens embedded in panel links. The panel API serves anything
	// beyond a bare hello only to holders of a valid ticket, so a stranger who finds the tunnel URL
	// can't even fetch the wrapped keys.
	tickets map[string]ticket
	running map[string]bool // integrations currently started
}

type ticket struct {
	purpose string
	expires time.Time
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
		tickets:     map[string]ticket{},
		running:     map[string]bool{},
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
		OnFinish: func(s approvals.Snapshot) {
			// Tell the agent how a panel approval ended, even if its turn is over.
			if s.Level == approvals.Strong && c.State() == Unlocked {
				c.Events.Emit("rubi", "approval.decided", map[string]any{"approval_id": s.ID, "kind": s.Kind,
					"state": s.State, "summary": s.Summary, "option": s.Chosen, "error": s.Error}, nil)
			}
		},
	})
	c.Events.OnEmit(c.deliverEvent)
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

// SetTransportError records why no panel connection can be offered (shown to the agent).
func (c *Core) SetTransportError(msg string) {
	c.mu.Lock()
	c.transErr = msg
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
		if c.transErr != "" {
			return "", errors.New(c.transErr)
		}
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
			c.pairCode, c.pairExp = randomToken(), time.Now().Add(pairingTTL)
		}
		q.Set("p", c.pairCode)
	} else {
		t := randomToken()
		c.pruneTicketsLocked()
		c.tickets[t] = ticket{purpose: purpose, expires: time.Now().Add(ticketTTL)}
		q.Set("t", t)
	}
	return c.PanelOrigin + "/#" + q.Encode(), nil
}

// MintTicket issues a ticket without a link, for handing to an already authenticated panel session.
func (c *Core) MintTicket(purpose string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := randomToken()
	c.pruneTicketsLocked()
	c.tickets[t] = ticket{purpose: purpose, expires: time.Now().Add(ticketTTL)}
	return t
}

// TicketPurpose returns the purpose of a valid ticket. Tickets stay valid for their whole lifetime
// (the user may unlock and then change settings within one panel visit).
func (c *Core) TicketPurpose(t string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneTicketsLocked()
	for k, v := range c.tickets {
		if subtle.ConstantTimeCompare([]byte(k), []byte(t)) == 1 {
			return v.purpose, true
		}
	}
	return "", false
}

func (c *Core) pruneTicketsLocked() {
	now := time.Now()
	for k, v := range c.tickets {
		if now.After(v.expires) {
			delete(c.tickets, k)
		}
	}
}

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
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
	c.stopIntegrations()
	c.Approvals.CancelAll()
	c.Events.Clear()
	c.Vault.Lock()
	c.Audit.SetVaultKey(nil)
	c.Audit.Record("rubi.locked", nil)
}

// Unlock opens the vault with a DEK delivered by the panel and records a receipt.
func (c *Core) Unlock(dek []byte, minVersion uint64, method, credentialID string) error {
	if c.State() == Unpaired {
		return errors.New("not paired")
	}
	if err := c.Vault.Unlock(dek, minVersion); err != nil {
		c.Audit.Record("rubi.unlock_failed", nil)
		return err
	}
	c.Audit.SetVaultKey(dek)
	c.addReceipt("unlocked", method, credentialID)
	c.Audit.Record("rubi.unlocked", audit.Fields{"method": method, "credential_id": credentialID})
	c.startIntegrations()
	return nil
}

// Keys returns the wrapped data keys (non-secret: only the user's device can open them).
func (c *Core) Keys() (*vault.Keys, error) {
	return vault.LoadKeys(c.Layout.Keys())
}

func (c *Core) addReceipt(event, method, credentialID string) {
	_ = c.Vault.Update(func(d *vault.Data) error {
		d.Receipts = append(d.Receipts, vault.Receipt{At: time.Now().UTC(), Event: event, Method: method,
			CredentialID: credentialID})
		if n := len(d.Receipts); n > vault.MaxReceipts {
			d.Receipts = d.Receipts[n-vault.MaxReceipts:]
		}
		return nil
	})
}

// Pair creates the vault on first run. Called by the panel API after the pairing code is verified.
func (c *Core) Pair(dek []byte, keys *vault.Keys, data *vault.Data, method, credentialID string) error {
	if err := c.pair(dek, keys, data); err != nil {
		return err
	}
	c.addReceipt("paired", method, credentialID)
	return nil
}

func (c *Core) pair(dek []byte, keys *vault.Keys, data *vault.Data) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paired {
		return errors.New("already paired")
	}
	if len(dek) != vault.KeySize {
		return errors.New("bad data key")
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
	if strings.HasPrefix(kind, "rubi.") {
		return approvals.Strong
	}
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
