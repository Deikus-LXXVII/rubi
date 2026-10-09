// Package core ties Rubi together: identity, vault, lock state, approvals, events, and panel links.
package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/audit"
	"github.com/Deikus-LXXVII/rubi/internal/events"
	"github.com/Deikus-LXXVII/rubi/internal/identity"
	"github.com/Deikus-LXXVII/rubi/internal/integrity"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/update"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/internal/version"
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

var ErrNoTransport = errors.New("the panel connection is not available yet (Rubi is still connecting to the relays); " +
	"try again in a few seconds")

type Core struct {
	Layout      paths.Layout
	ID          *identity.Identity
	Vault       *vault.Store
	Approvals   *approvals.Engine
	Events      *events.Store
	Audit       *audit.Log
	Store       *plugins.Store
	Runner      *plugins.Runner
	PanelOrigin string
	// HookBase is the public base URL of hook addresses ("" = none on this transport), and
	// OnHookRoute applies a new hook route to the transport. Both are set by the daemon.
	HookBase    func() string
	OnHookRoute func()

	mu             sync.Mutex
	paired         bool
	downgradedFrom string // see checkDowngrade
	endpoint       string // current public URL of the panel API (set by the tunnel or RUBI_PUBLIC_URL)
	relayPub       string // Rubi's routing key on the relays ("" = relay transport off)
	relays         []string
	relayUp        int // relays currently connected
	// transport is the user's chosen transport (gateway, relays, tailscale).
	transport string
	transErr  string // why there is no endpoint, if the transport can't start
	pairCode  string
	pairExp   time.Time
	// tickets are short-lived random tokens embedded in panel links. The panel API serves anything
	// beyond a bare hello only to holders of a valid ticket, so a stranger who finds the tunnel URL
	// can't even fetch the wrapped keys.
	tickets map[string]ticket
	plans   map[string]pendingPlan // approval id -> what the agent will do once it is decided
	seen    map[string]bool        // approvals whose outcome the agent already got from rubi_approval
	upd     updateState
	mkt     marketState
	integ   integrity.Result
	hookHit map[string][]time.Time // recent deliveries per hook, for rate limiting

	agentAuth agentAuth // codes that prove which Bot is calling (agentauth.go)
	devs      deviceClients
}

// SetIntegrity records the result of the binary self-check.
func (c *Core) SetIntegrity(r integrity.Result) {
	c.mu.Lock()
	c.integ = r
	c.mu.Unlock()
}

// Integrity returns the last self-check result.
func (c *Core) Integrity() integrity.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.integ.Status == "" {
		return integrity.Result{Status: "unknown", Detail: "check not finished yet"}
	}
	return c.integ
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
	store, err := plugins.OpenStore(layout.Plugins())
	if err != nil {
		return nil, err
	}
	c := &Core{
		Layout:      layout,
		ID:          id,
		Vault:       vault.NewStore(layout.Vault(), id.InstanceID),
		Events:      events.NewStore(),
		Audit:       audit.Open(layout.Audit()),
		Store:       store,
		PanelOrigin: DefaultPanelOrigin,
		tickets:     map[string]ticket{},
		plans:       map[string]pendingPlan{},
		seen:        map[string]bool{},
	}
	c.Runner = &plugins.Runner{Store: store, LogDir: layout.Logs(), RubiVersion: version.Version, Hooks: plugins.Hooks{
		Handler: c.pluginHandler, Launched: c.pluginLaunched, Crashed: c.pluginCrashed, Logf: log.Printf}}
	_, keysErr := os.Stat(layout.Keys())
	c.paired = keysErr == nil && c.Vault.Exists()
	c.PanelOrigin = panelOrigin(layout, c.paired)
	c.Approvals = approvals.New(approvals.DefaultConfig(), approvals.Hooks{
		Level: c.PolicyLevel,
		Link:  func(id string) (string, error) { return c.Link("approve:" + id) },
		Label: c.Label,
		Audit: func(ev string, f map[string]any) { c.Audit.Record(ev, f) },
		OnFinish: func(s approvals.Snapshot) {
			// Tell the agent how a panel approval ended, even if its turn is over, with the plan it left
			// for this moment (rubi_continue_after), so a fresh routine run can pick the task up.
			//
			// The agent always learns the outcome, unless it already saw it while waiting in rubi_approval.
			// It goes to the Bot that left a plan, else to the Bots following Rubi's events (or the default).
			p := c.takePlan(s.ID)
			if s.Level != approvals.Strong || c.State() != Unlocked {
				return
			}
			if s.Kind == "rubi.update" && s.State == approvals.Executed {
				return // Rubi restarts; the new process reports update.completed instead
			}
			time.Sleep(seenGrace)
			if c.takeSeen(s.ID) {
				return
			}
			data := map[string]any{"approval_id": s.ID, "kind": s.Kind, "state": s.State,
				"summary": s.Summary, "option": s.Chosen, "error": s.Error, "result": s.Result}
			if p.plan != "" {
				data["your_plan"] = p.plan
			}
			c.Events.EmitFor(p.agent, false, "rubi", "approval.decided", data, []string{"result"})
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

// SetRelay records the relay transport's routing key and relays (links carry them).
func (c *Core) SetRelay(pub string, relays []string) {
	c.mu.Lock()
	c.relayPub, c.relays = pub, append([]string(nil), relays...)
	c.mu.Unlock()
}

// SetRelaysConnected is called by the relay transport whenever the number of connected relays changes.
func (c *Core) SetRelaysConnected(n int) {
	c.mu.Lock()
	c.relayUp = n
	c.mu.Unlock()
}

// SetTransportName records the user's chosen transport (shown in rubi_status).
func (c *Core) SetTransportName(t string) {
	c.mu.Lock()
	c.transport = t
	c.mu.Unlock()
}

// TransportInfo describes how the panel is reached right now.
func (c *Core) TransportInfo() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	info := map[string]any{"transport": c.transport, "relays_connected": c.relayUp, "relays": len(c.relays)}
	if c.endpoint != "" {
		info["https"] = true
	}
	if c.transErr != "" && c.endpoint == "" && c.relayUp == 0 {
		info["problem"] = c.transErr
	}
	return info
}

// RelaysConnected reports how many relays carry the panel right now.
func (c *Core) RelaysConnected() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.relayUp
}

func sameRelays(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	if err := c.validPurpose(purpose); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.endpoint == "" && c.relayUp == 0 {
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
	if c.endpoint != "" {
		q.Set("e", c.endpoint)
	}
	if c.relayPub != "" {
		q.Set("n", c.relayPub)
		if !sameRelays(c.relays, relay.DefaultRelays) {
			q.Set("r", strings.Join(c.relays, ","))
		}
	}
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

func (c *Core) validPurpose(p string) error {
	switch {
	case p == "pair", p == "unlock", p == "settings":
		return nil
	case strings.HasPrefix(p, "setup:"):
		if _, ok := c.Store.Get(strings.TrimPrefix(p, "setup:")); ok {
			return nil
		}
		return errors.New("plugin " + strings.TrimPrefix(p, "setup:") + " is not installed; see rubi_store")
	case strings.HasPrefix(p, "approve:"):
		return nil
	case strings.HasPrefix(p, "agent:"):
		_, err := CleanAgentName(strings.TrimPrefix(p, "agent:"))
		return err
	}
	return errors.New(`purpose must be "pair", "unlock", "settings", "setup:<plugin id>" or "agent:<your Bot name>"`)
}

// Lock wipes keys and private data from memory and cancels pending approvals. Always allowed.
func (c *Core) Lock() {
	c.stopPlugins()
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
	c.checkPanelOrigin()
	c.checkDowngrade()
	c.addReceipt("unlocked", method, credentialID)
	c.Audit.Record("rubi.unlocked", audit.Fields{"method": method, "credential_id": credentialID})
	c.startPlugins()
	if c.OnHookRoute != nil {
		c.OnHookRoute()
	}
	c.maybeNotifyUpdate()
	go c.CheckPluginUpdates(context.Background())
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
	c.maybeNotifyUpdate()
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
	data.PanelOrigin = c.PanelOrigin
	if err := c.Vault.Create(dek, data); err != nil {
		return err
	}
	c.paired, c.pairCode = true, ""
	_ = vault.WriteFileAtomic(c.Layout.PanelOrigin(), []byte(c.PanelOrigin+"\n"), 0o600)
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

// PolicyLevel resolves the approval level for an action kind: the user's policy from the vault, then the
// plugin's default. Core (rubi.*) and unknown kinds are always strong.
func (c *Core) PolicyLevel(kind string) approvals.Level {
	if strings.HasPrefix(kind, "rubi.") {
		return approvals.Strong
	}
	a, _, ok := c.action(kind)
	if !ok {
		return approvals.Strong
	}
	level := approvals.Level(a.DefaultLevel)
	if a.Locked {
		return level
	}
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

// Endpoint is the current public URL of the panel API ("" when Rubi is reached only through relays).
func (c *Core) Endpoint() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.endpoint
}

// panelOrigin decides where links point. RUBI_PANEL_ORIGIN (self-hosted panels) counts only until the
// instance is paired; after that the origin it was paired with is fixed, so an injected instruction to
// restart Rubi with another origin can't turn every unlock link into a phishing page. Test builds may
// always override it.
func panelOrigin(layout paths.Layout, paired bool) string {
	return panelOriginFor(layout, paired, version.Version)
}

func panelOriginFor(layout paths.Layout, paired bool, ver string) string {
	env := strings.TrimRight(os.Getenv("RUBI_PANEL_ORIGIN"), "/")
	if env != "" && (ver == "dev" || strings.HasSuffix(ver, "-test")) {
		return env
	}
	if paired {
		if b, err := os.ReadFile(layout.PanelOrigin()); err == nil {
			if o := strings.TrimSpace(string(b)); o != "" {
				if env != "" && env != o {
					log.Printf("RUBI_PANEL_ORIGIN ignored: this Rubi was paired with %s", o)
				}
				return o
			}
		}
		if env != "" && env != DefaultPanelOrigin {
			log.Printf("RUBI_PANEL_ORIGIN ignored: it can only be set before pairing")
		}
		return DefaultPanelOrigin
	}
	if env != "" {
		return env
	}
	return DefaultPanelOrigin
}

// checkPanelOrigin compares the origin in use with the one sealed in the vault at pairing. A mismatch
// means the plain copy was edited: Rubi goes back to the sealed one and records it.
func (c *Core) checkPanelOrigin() {
	var sealed string
	_ = c.Vault.Update(func(d *vault.Data) error {
		if d.PanelOrigin == "" { // paired before the origin was sealed
			d.PanelOrigin = c.PanelOrigin
		}
		sealed = d.PanelOrigin
		return nil
	})
	if sealed == "" || sealed == c.PanelOrigin {
		return
	}
	c.Audit.Record("rubi.panel_origin_restored", audit.Fields{"found": c.PanelOrigin, "restored": sealed})
	c.mu.Lock()
	c.PanelOrigin = sealed
	c.mu.Unlock()
	_ = vault.WriteFileAtomic(c.Layout.PanelOrigin(), []byte(sealed+"\n"), 0o600)
}

// checkDowngrade remembers the newest Rubi ever unlocked and reports when an older one is unlocked
// (rubi rollback, or a replaced binary): the user sees it on the panel and the agent is told.
func (c *Core) checkDowngrade() {
	cur := version.Version
	if cur == "dev" || strings.HasSuffix(cur, "-test") {
		return
	}
	var highest string
	_ = c.Vault.Update(func(d *vault.Data) error {
		if d.HighestVersion == "" || update.Newer(cur, d.HighestVersion) {
			d.HighestVersion = cur
		}
		highest = d.HighestVersion
		return nil
	})
	c.mu.Lock()
	c.downgradedFrom = ""
	if update.Newer(highest, cur) {
		c.downgradedFrom = highest
	}
	from := c.downgradedFrom
	c.mu.Unlock()
	if from != "" {
		c.Audit.Record("rubi.downgraded", audit.Fields{"running": cur, "highest": from})
		c.Events.Emit("rubi", "rubi.downgraded", map[string]any{"running": cur, "highest": from,
			"next_step": "Tell the user Rubi is running an older version than before (" + cur + ", was " + from + "). If they didn't roll back on purpose, they should update (rubi_update)."}, nil)
	}
}

// DowngradedFrom is the newer version this Rubi ran before, when it now runs an older one ("" otherwise).
func (c *Core) DowngradedFrom() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.downgradedFrom
}
