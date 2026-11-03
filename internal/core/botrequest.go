package core

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/audit"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// A Bot's own requests. A Bot can ask, through Rubi's MCP tools, to be connected (its routine's webhook
// address and key) and to be given plugin accounts (e.g. a mailbox), instead of the user setting this up
// in the panel. Nothing changes until the user approves in the panel with a passkey or password: the
// request is a strong approval like any other settings change, and the agent cannot complete it.
//
//   - One pending request per Bot. A second call while one waits merges into it: the waiting approval is
//     cancelled and one new approval carries everything (a newer webhook replaces the older one).
//   - Several things become one batch approval; the user may leave any of them out.
//   - The webhook key is sealed in memory (XChaCha20-Poly1305 under a random per-process key) while it
//     waits. It is never written to disk, logged, shown in the panel or returned to an agent. On approval
//     it goes into the vault (encrypted like every other secret); on denial, expiry, cancellation, a merge
//     or when Rubi locks, it is wiped.
//   - Changing an existing Bot's webhook says so ("replaces", highlighted), and the change only applies if
//     that webhook is still what the user saw when the request was made.

const (
	botRequestKind        = "rubi.bot.request"
	botRequestsPerHour    = 10 // per Bot name
	botRequestsAllPerHour = 30 // all Bots together
	maxBotAccessItems     = 20
	maxBotReason          = 300
	botWebhookTestWait    = 12 * time.Second // within the relays' 30 s, as the approval waits for it
	webhookItem           = "webhook"
	// The inputs of the approval screen when the Bot couldn't pass its webhook key (a Grok Bot never sees it).
	inputWebhookURL = "webhook_url"
	inputWebhookKey = "webhook_key"
)

// WebhookProposal is the routine webhook a Bot asks Rubi to wake it through. Without a key (a Grok Bot
// can't read its routine's key) the user pastes the address and key on the approval screen; a URL the Bot
// passes is only a prefill there.
type WebhookProposal struct {
	URL    string
	Key    string
	Events []string // sources the Bot wants to hear about; nil keeps the current choice
}

// PluginAccessWant is a plugin whose accounts a Bot asks to be given (no accounts: all connected ones).
type PluginAccessWant struct {
	Plugin   string   `json:"id"`
	Accounts []string `json:"accounts,omitempty"`
}

// BotRequest is what a Bot asks for itself in one call.
type BotRequest struct {
	Agent   string
	Webhook *WebhookProposal
	Access  []PluginAccessWant
	Reason  string
}

// botItem is one thing a waiting request asks for: the webhook, or one plugin account.
type botItem struct {
	key     string // "webhook" or "access:<plugin>|<account>"
	label   string
	preview map[string]any

	url      string
	sealed   []byte // the webhook key, sealed with botRequests.aead
	paste    bool   // no key from the Bot: the user pastes address and key on the approval screen
	subs     []string
	subsSet  bool
	current  string // the Bot's webhook when the request was made ("" = none), checked again on approval
	replaces bool

	plugin, account string
	once            bool     // a one-off access (rubi.access): lasts as long as the user chooses, not for good
	res             resolved // the account of a one-off item
}

// onceAsk is a Bot's one-off access ask (rubi.access) that travels with its request: the accounts it wants for
// a task (items), the ones it is assigned to already (granted), and how long it asked for. It keeps the meaning it
// has in its own approval: the user picks the duration, and nothing in it is permanent unless they pick "always".
type onceAsk struct {
	items   []*botItem
	granted []resolved
	task    string
	d       time.Duration
}

func (o *onceAsk) has(key string) bool {
	for _, it := range o.items {
		if it.key == key {
			return true
		}
	}
	return false
}

// pendingOnce is a one-off access approval that waits on its own, so a request of the same Bot that comes
// later can join it.
type pendingOnce struct {
	approvalID string
	once       *onceAsk
}

type botBundle struct {
	agent      string
	approvalID string
	items      []*botItem // the webhook first, then the permanent accounts
	once       *onceAsk   // the Bot's one-off access, if it joined this request
	reason     string
	timer      *time.Timer
}

type botRequests struct {
	propose sync.Mutex // one proposal at a time, so merges don't race
	mu      sync.Mutex
	aead    cipher.AEAD
	byID    map[string]*botBundle
	byAgent map[string]*botBundle   // by lower-cased name
	onceBy  map[string]*pendingOnce // waiting one-off access approvals, by lower-cased Bot name
}

func (b *botRequests) cipher() cipher.AEAD {
	if b.aead == nil {
		k := make([]byte, chacha20poly1305.KeySize)
		_, _ = rand.Read(k)
		b.aead, _ = chacha20poly1305.NewX(k)
		b.byID, b.byAgent, b.onceBy = map[string]*botBundle{}, map[string]*botBundle{}, map[string]*pendingOnce{}
	}
	return b.aead
}

// seal and open bind the key to the Bot and address it was proposed with.
func (b *botRequests) seal(key, ad string) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.cipher()
	nonce := make([]byte, a.NonceSize())
	_, _ = rand.Read(nonce)
	return a.Seal(nonce, nonce, []byte(key), []byte(ad))
}

func (b *botRequests) open(sealed []byte, ad string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.cipher()
	if len(sealed) < a.NonceSize() {
		return nil, errors.New("the webhook key of this request is gone; ask the Bot to send it again")
	}
	p, err := a.Open(nil, sealed[:a.NonceSize()], sealed[a.NonceSize():], []byte(ad))
	if err != nil {
		return nil, errors.New("the webhook key of this request is gone; ask the Bot to send it again")
	}
	return p, nil
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func sealAD(agent, rawURL string) string { return strings.ToLower(agent) + "\x00" + rawURL }

// webhookHostPath shows a webhook as the user can check it against the routine: scheme, host and path,
// without the query (it can carry a token) and with a very long path shortened in the middle.
func webhookHostPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid)"
	}
	p := u.EscapedPath()
	if len(p) > 80 {
		p = p[:44] + "…" + p[len(p)-32:]
	}
	s := u.Scheme + "://" + u.Host + p
	if u.RawQuery != "" {
		s += " (+ a hidden query)"
	}
	return s
}

// ProposeBot records what a Bot asks for itself as a pending strong approval (merged with the Bot's
// waiting one, if any) and returns what the Bot needs: the approval id and a one-time panel link.
func (c *Core) ProposeBot(ctx context.Context, r BotRequest) (map[string]any, error) {
	return c.proposeBot(ctx, r, nil)
}

// proposeBot is ProposeBot with a one-off access ask that joins the request (from RequestAccess). Without one, a
// one-off access of the same Bot that waits on its own is taken in.
func (c *Core) proposeBot(ctx context.Context, r BotRequest, once *onceAsk) (map[string]any, error) {
	if c.State() != Unlocked {
		return nil, errors.New("Rubi is locked: send the user rubi_link(\"unlock\") first, then ask again")
	}
	name, err := CleanAgentName(r.Agent)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(r.Reason)
	if len([]rune(reason)) > maxBotReason {
		return nil, fmt.Errorf("the reason is too long (at most %d characters)", maxBotReason)
	}
	if r.Webhook == nil && len(r.Access) == 0 && once == nil {
		return nil, errors.New("nothing to ask for: pass a webhook (url and key) and/or plugins")
	}
	if len(r.Access) > maxBotAccessItems {
		return nil, fmt.Errorf("ask for at most %d plugins at once", maxBotAccessItems)
	}

	var items []*botItem
	var already []string
	var known bool // the Bot is connected already
	if r.Webhook != nil {
		it, err := c.webhookItem(name, *r.Webhook)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	_ = c.Vault.View(func(d *vault.Data) error {
		if a := findAgent(d, name); a != nil {
			known, name = true, a.Name
		}
		return nil
	})
	acc, done, err := c.accessItems(name, r.Access)
	if err != nil {
		return nil, err
	}
	items, already = append(items, acc...), done

	b := &c.botReqs
	b.propose.Lock()
	defer b.propose.Unlock()

	hasWebhook := len(items) > 0 && items[0].key == webhookItem
	if !c.allowHourly("botreq:"+strings.ToLower(name), botRequestsPerHour) || !c.allowHourly("botreq", botRequestsAllPerHour) {
		for _, it := range items {
			wipe(it.sealed)
		}
		return nil, errors.New("too many requests from Bots this hour; wait, or let the user set this up in the panel")
	}

	// Merge with the request of this Bot that is still waiting: cancel it first (so the user can't approve
	// both), then carry its items over.
	var merged *botBundle
	b.mu.Lock()
	b.cipher()
	old := b.byAgent[strings.ToLower(name)]
	if old != nil {
		delete(b.byID, old.approvalID) // its items move; cancelling it must not wipe them
		delete(b.byAgent, strings.ToLower(name))
		if old.timer != nil {
			old.timer.Stop()
		}
	}
	b.mu.Unlock()
	if old != nil {
		if s, err := c.Approvals.Cancel(old.approvalID); err == nil && s.State == approvals.Cancelled {
			merged = old
		} else { // already decided or gone: nothing to carry over
			for _, it := range old.items {
				wipe(it.sealed)
			}
		}
	}
	if merged != nil {
		have := map[string]bool{}
		for _, it := range items {
			have[it.key] = true
		}
		var kept []*botItem
		for _, it := range merged.items {
			if have[it.key] {
				wipe(it.sealed) // superseded by the newer request
				continue
			}
			kept = append(kept, it)
		}
		items = mergeItems(items, kept)
		hasWebhook = len(items) > 0 && items[0].key == webhookItem
		if reason == "" {
			reason = merged.reason
		} else if merged.reason != "" && merged.reason != reason {
			reason = merged.reason + " / " + reason
			if len([]rune(reason)) > 2*maxBotReason {
				reason = string([]rune(reason)[:2*maxBotReason])
			}
		}
	}
	if merged != nil && merged.once != nil { // the waiting request carried a one-off access: keep it
		var err error
		if once, err = combineOnce(merged.once, once); err != nil {
			for _, it := range items {
				wipe(it.sealed)
			}
			return nil, fmt.Errorf("%w (your earlier request %s was cancelled; ask again with everything)", err, merged.approvalID)
		}
	}
	fail := func(err error) (map[string]any, error) {
		for _, it := range items {
			wipe(it.sealed)
		}
		if merged != nil {
			err = fmt.Errorf("%w (your earlier request %s was cancelled; ask again with everything)", err, merged.approvalID)
		}
		return nil, err
	}
	if !known && !hasWebhook && (len(items) > 0 || once != nil) {
		return fail(fmt.Errorf("%s isn't connected to Rubi yet: ask for your webhook first (rubi_agent_connect), "+
			"plugins can go in the same request", name))
	}
	if len(items) == 0 && once != nil {
		// The request this one-off access was to join was decided between the check and now: it is an access
		// request on its own again.
		return c.submitOnceAloneFor(ctx, name, once)
	}
	if len(items) == 0 {
		return map[string]any{"status": "already_allowed", "already_allowed": already,
			"message": name + " already has everything it asked for; nothing for the user to approve."}, nil
	}

	// A one-off access of this Bot that waits on its own joins this card (one passkey for both). It is cancelled
	// only once the card exists, so a failed submit leaves the user's waiting card alone.
	var absorbed *pendingOnce
	if once == nil {
		if absorbed = c.peekPendingOnce(name); absorbed != nil {
			once = absorbed.once
		}
	}
	var dropped []string // one-off accounts the card keeps for good already
	if once != nil {
		cp := &onceAsk{task: once.task, d: once.d, granted: once.granted} // a copy: the waiting ask is not changed
		for _, it := range once.items {
			dup := false
			for _, p := range items {
				dup = dup || p.key == "access:"+strings.TrimPrefix(it.key, "once:")
			}
			if dup {
				dropped = append(dropped, it.res.title())
			} else {
				cp.items = append(cp.items, it)
			}
		}
		// With no item left but accounts the Bot is assigned to already, the ask stays: approving the card still
		// gives the task code for those accounts.
		if once = cp; len(cp.items) == 0 && len(cp.granted) == 0 {
			once = nil
		}
	}
	bundle := &botBundle{agent: name, items: items, reason: reason, once: once}
	req := c.botApproval(bundle, already)
	out, err := c.Approvals.Submit(ctx, req)
	if err != nil {
		return fail(err)
	}
	if absorbed != nil {
		if s, err := c.Approvals.Cancel(absorbed.approvalID); err != nil || s.State != approvals.Cancelled {
			// The user decided that access request just now: this card would repeat it.
			if id, _ := out["approval_id"].(string); id != "" {
				_, _ = c.Approvals.Cancel(id)
			}
			return fail(errors.New("the user has just decided your waiting one-off access request, so this request was not sent; ask again for anything it did not cover"))
		}
		b.mu.Lock()
		delete(b.onceBy, strings.ToLower(name))
		b.mu.Unlock()
	}
	bundle.approvalID, _ = out["approval_id"].(string)
	expires, _ := time.Parse(time.RFC3339, fmt.Sprint(out["expires_at"]))
	b.mu.Lock()
	b.byID[bundle.approvalID], b.byAgent[strings.ToLower(name)] = bundle, bundle
	id := bundle.approvalID
	bundle.timer = time.AfterFunc(time.Until(expires)+time.Second, func() { c.expireBotRequest(id) })
	b.mu.Unlock()
	if merged != nil {
		out["merged"] = merged.approvalID
		out["merge_note"] = "Your earlier request (" + merged.approvalID + ") is folded into this one; its link no longer works. Send the user this link instead."
	}
	c.Audit.Record("bot.request", audit.Fields{"approval_id": id, "agent": name, "items": len(items), "merged": merged != nil})

	var labels []string
	replaces := false
	for _, it := range bundle.allItems() {
		labels = append(labels, bundle.labelOf(it))
		replaces = replaces || it.replaces
	}
	res := map[string]any{"status": "awaiting_approval", "approval_id": id, "level": "strong",
		"asks_for": labels, "replaces_existing_webhook": replaces, "expires_at": out["expires_at"]}
	if u, ok := out["approval_url"]; ok {
		res["approval_url"] = u
	} else if e, ok := out["approval_url_error"]; ok {
		res["approval_url_error"] = e
	}
	if absorbed != nil {
		res["merged_access"] = absorbed.approvalID
		res["merge_note"] = "Your waiting one-off access request (" + absorbed.approvalID + ") is folded into this one; its link no longer works. Send the user this link instead."
	}
	if len(dropped) > 0 {
		res["access_note"] = "Your one-off access for " + strings.Join(dropped, ", ") + " was dropped from this card because the card already keeps those accounts for good."
		if once == nil {
			res["access_note"] = res["access_note"].(string) + " Nothing is left of the one-off request."
		}
	}
	if once != nil && len(once.items) > 0 {
		res["instructions_access"] = "This card also holds your one-off access for \"" + once.task + "\": the user picks how long it lasts " +
			"(this task, a day, a week or always). What you asked for to keep is for good."
	}
	for _, k := range []string{"merged", "merge_note"} {
		if v, ok := out[k]; ok {
			res[k] = v
		}
	}
	if len(already) > 0 {
		res["already_allowed"] = already
	}
	res["instructions"] = "Send the user approval_url in one short message saying what you asked for. They approve " +
		"in the Rubi panel with their passkey (you cannot approve it) and may leave parts out. Then call " +
		"rubi_approval with wait_seconds. If you asked for your webhook, a webhook.test event reaches you once it works; " +
		"check rubi_status."
	if hasWebhook && !items[0].paste {
		res["instructions"] = res["instructions"].(string) + " Rubi already deleted your key_file; if you passed key directly, " +
			"delete any copy you wrote (shred -u)."
	}
	if hasWebhook && items[0].paste {
		res["user_pastes_webhook"] = true
		res["instructions"] = "Send the user ONE message with approval_url and the deep link to your routine \"Rubi events\"'s " +
			"webhook field (from <automation_status>). They open both in the Grok Bot desktop app, paste the routine's " +
			"Webhook section (POST to and header) into the Rubi approval screen and approve with their passkey; they may " +
			"leave parts out. You cannot approve it and never see the key. Then call rubi_approval with wait_seconds; a " +
			"webhook.test event reaches you once it works; check rubi_status."
	}
	return res, nil
}

// mergeItems keeps the webhook first, then the plugin accounts in the order asked.
func mergeItems(newer, older []*botItem) []*botItem {
	var hook *botItem
	var rest []*botItem
	for _, it := range append(append([]*botItem{}, newer...), older...) {
		if it.key == webhookItem {
			if hook == nil {
				hook = it
			}
			continue
		}
		rest = append(rest, it)
	}
	if hook != nil {
		return append([]*botItem{hook}, rest...)
	}
	return rest
}

// botWebhookURL refuses what a Bot may not propose: anything but a Grok Bot routine webhook. (The user's
// own Connect page and settings still accept other receivers, with a warning.)
func (c *Core) botWebhookURL(raw string) error {
	if err := validWebhookURL(raw); err != nil {
		return err
	}
	if c.BotWebhookOK != nil && c.BotWebhookOK(raw) {
		return nil
	}
	if !isRoutineWebhook(raw) {
		return errors.New("only a Grok Bot routine webhook (https://" + routineHost + routinePathPrefix +
			"<id>) can be proposed by a Bot; ask the user for the routine's \"POST to\" address, or connect another receiver in Rubi's settings")
	}
	return nil
}

// webhookItem checks a proposed webhook and seals its key.
func (c *Core) webhookItem(name string, w WebhookProposal) (*botItem, error) {
	w.URL, w.Key = strings.TrimSpace(w.URL), strings.TrimSpace(w.Key)
	if w.URL != "" {
		if err := c.botWebhookURL(w.URL); err != nil {
			return nil, err
		}
	}
	var key string
	var err error
	if strings.HasPrefix(w.Key, "$file:") { // the agent's file substitution didn't happen: never store the path as a key
		return nil, errors.New("the key arrived as a $file: reference, not its contents. " + keyFileHint)
	}
	if w.Key != "" {
		if w.URL == "" {
			return nil, errors.New("a webhook key needs its url; or pass neither and the user pastes both on the approval screen")
		}
		if key, err = botWebhookKey(w.Key, w.URL); err != nil {
			return nil, err
		}
	}
	it := &botItem{key: webhookItem, url: w.URL, paste: w.Key == ""}
	if w.Events != nil {
		if it.subs, err = c.cleanSources(w.Events); err != nil {
			return nil, err
		}
		it.subsSet = true
	}
	_ = c.Vault.View(func(d *vault.Data) error {
		if a := findAgent(d, name); a != nil {
			name, it.current = a.Name, a.URL
		}
		return nil
	})
	it.replaces = it.current != ""
	p := map[string]any{"agent": name,
		"proposed_by": "A Bot, through Rubi's tools (not you). Approve only if you asked a Bot to connect to Rubi."}
	shown := ""
	switch {
	case !it.paste:
		shown = webhookHostPath(w.URL)
		p["webhook"] = shown
		if !strings.HasPrefix(key, routineKeyPrefix) {
			p["warning"] = "This key doesn't look like a Grok Bot routine key (" + routineKeyPrefix +
				"…). Approve only if you set up this receiver yourself."
		}
	default:
		p["webhook"] = "You paste it below, from " + name + "'s routine \"Rubi events\" (Webhook section: POST to and header). " +
			"The Bot never sees the key."
		if w.URL != "" {
			shown = webhookHostPath(w.URL)
			p["webhook"] = "Suggested by the Bot: " + shown + ". Check it against the routine, or paste the routine's own."
		}
	}
	if it.replaces {
		what := "the webhook " + webhookHostPath(it.current)
		if it.current == w.URL && !it.paste {
			what = "the key of the webhook " + webhookHostPath(it.current) + " (same address, new key)"
		}
		to := "the address above"
		if it.paste {
			to = "the address you paste"
		}
		p["replaces"] = "REPLACES " + what + ". From now on " + name + "'s events, including approval results and access codes, go to " + to + "."
		it.label = "Replace " + name + "'s webhook"
		if shown != "" && !it.paste {
			it.label += " with " + shown
		}
	} else {
		it.label = "Connect " + name
		if !it.paste {
			it.label += " (webhook " + shown + ")"
		} else {
			it.label += " (you paste its webhook)"
		}
	}
	if it.subsSet {
		p["notifies_about"] = c.describeSources(it.subs)
	}
	it.preview = p
	if !it.paste {
		it.sealed = c.botReqs.seal(key, sealAD(name, w.URL))
	}
	return it, nil
}

// accessItems resolves the plugin accounts a Bot asks for. Accounts it already has are reported, not asked.
func (c *Core) accessItems(name string, want []PluginAccessWant) (items []*botItem, already []string, err error) {
	seen := map[string]bool{}
	err = c.Vault.View(func(d *vault.Data) error {
		for _, w := range want {
			id := strings.TrimSpace(w.Plugin)
			m, ok := c.Store.Get(id)
			if !ok {
				return fmt.Errorf("plugin %q is not installed (find it with rubi_store, install it with rubi_plugin_install)", id)
			}
			i := d.Integrations[id]
			if i == nil || len(i.Accounts) == 0 {
				return fmt.Errorf("%s is not connected yet: send the user rubi_link(\"setup:%s\") first", m.Name, id)
			}
			var accts []*vault.Account
			if len(w.Accounts) == 0 {
				accts = i.Accounts
			} else {
				for _, ref := range w.Accounts {
					a, err := accountOf(d, id, ref)
					if err != nil {
						return fmt.Errorf("%s: %w", m.Name, err)
					}
					accts = append(accts, a)
				}
			}
			for _, a := range accts {
				r := resolved{plugin: m.ID, name: m.Name, account: a.ID, label: a.Label}
				if seen[r.key()] {
					continue
				}
				seen[r.key()] = true
				assigned := false
				for _, n := range a.Agents {
					assigned = assigned || strings.EqualFold(n, name)
				}
				if assigned {
					already = append(already, r.title())
					continue
				}
				var can []string
				for _, act := range m.Actions {
					can = append(can, act.Title+" ("+string(act.DefaultLevel)+" approval)")
				}
				for _, t := range m.Tools {
					can = append(can, t.Name)
				}
				p := map[string]any{"bot": name, "plugin": m.Name, "account": a.Label,
					"effect": name + " may use this account whenever it asks, for good (actions that need your approval still do). You can take it back in Settings, " + m.Name + ", Bots with access."}
				if len(can) > 0 {
					p["can"] = strings.Join(can, "; ")
				}
				if len(m.Egress) > 0 {
					p["connects_to"] = strings.Join(m.Egress, ", ")
				}
				items = append(items, &botItem{key: "access:" + r.key(), label: "Let " + name + " use " + r.title(),
					preview: p, plugin: m.ID, account: a.ID})
			}
		}
		return nil
	})
	return items, already, err
}

// botApproval builds the approval for a bundle: one item, or a batch where each item can be left out.
func (c *Core) botApproval(bundle *botBundle, already []string) approvals.Request {
	name := bundle.agent
	preview := map[string]any{"bot": name}
	if bundle.reason != "" {
		preview["reason"] = bundle.reason
	}
	if len(already) > 0 {
		preview["already_allowed"] = strings.Join(already, ", ")
	}
	all := bundle.allItems()
	req := approvals.Request{Integration: "rubi", Kind: botRequestKind,
		Options: []approvals.Option{{Key: "apply", Label: "Approve"}},
		Execute: func(ctx context.Context, option string) (any, error) { return c.applyBotRequest(ctx, bundle, option) }}
	if hook := bundle.items[0]; hook.key == webhookItem && hook.paste {
		item := ""
		if len(all) > 1 {
			item = webhookItem
		}
		req.Inputs = []approvals.Input{
			{Key: inputWebhookURL, Label: "POST to", Value: hook.url, Item: item,
				Help: "From " + name + "'s routine \"Rubi events\", Webhook section. Paste the whole section if you like."},
			{Key: inputWebhookKey, Label: "Header (key)", Secret: true, Item: item,
				Help: "The routine's key (crsr_…). It stays in Rubi; nobody else sees it."},
		}
		req.CheckInputs = func(option string, in map[string]string) error {
			if !webhookChosen(option) {
				return nil
			}
			_, _, err := c.pastedWebhook(in)
			return err
		}
	}
	if len(all) == 1 {
		it := bundle.items[0]
		for k, v := range it.preview {
			preview[k] = v
		}
		req.Summary = it.label
	} else {
		var labels []string
		for _, it := range all {
			labels = append(labels, bundle.labelOf(it))
			req.Items = append(req.Items, approvals.Item{Key: it.key, Label: bundle.labelOf(it), Preview: it.preview})
			if it.replaces { // the replacement is marked on the whole request too, not only on its item
				preview["replaces"] = it.preview["replaces"]
			}
			if w, ok := it.preview["warning"]; ok {
				preview["warning"] = w
			}
		}
		preview["proposed_by"] = "A Bot, through Rubi's tools (not you). Untick anything you didn't ask for."
		req.Summary = fmt.Sprintf("%s asks for %d changes", name, len(all))
		req.Question = "Which of these may " + name + " have?"
		if bundle.hasOnceItems() {
			// The duration belongs to the items marked "for the time you choose" only; the others are for good.
			preview["task"] = bundle.once.task
			req.Options = accessOptions(bundle.once.d)
			req.Question = "How long may " + name + " use the accounts marked \u201cfor the time you choose\u201d? Everything else here is for good."
		}
	}
	req.Preview = preview
	return req
}

// webhookChosen: the option includes the webhook item (or isn't a batch).
func webhookChosen(option string) bool {
	_, keys := approvals.SplitOption(option)
	if keys == nil {
		return true
	}
	for _, k := range keys {
		if k == webhookItem {
			return true
		}
	}
	return false
}

// pastedWebhook checks what the user pasted on the approval screen, like any pasted webhook.
func (c *Core) pastedWebhook(in map[string]string) (rawURL, key string, err error) {
	rawURL = strings.TrimSpace(in[inputWebhookURL])
	if rawURL == "" {
		return "", "", errors.New("paste the routine's \"POST to\" address")
	}
	if err := c.botWebhookURL(rawURL); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(in[inputWebhookKey]) == "" {
		return "", "", errors.New("paste the routine's header (its key)")
	}
	if key, err = cleanRoutineKey(in[inputWebhookKey], rawURL); err != nil {
		return "", "", err
	}
	return rawURL, key, nil
}

// applyBotRequest runs on the user's approval (DecidePanel, after their proof was checked).
func (c *Core) applyBotRequest(ctx context.Context, bundle *botBundle, option string) (any, error) {
	duration, keys := approvals.SplitOption(option) // the duration is the user's choice for the one-off items only
	chosen := map[string]bool{}
	for _, k := range keys {
		chosen[k] = true
	}
	var pick, pickOnce []*botItem
	for _, it := range bundle.items {
		if keys == nil || chosen[it.key] {
			pick = append(pick, it)
		}
	}
	if bundle.once != nil {
		for _, it := range bundle.once.items {
			if keys == nil || chosen[it.key] {
				pickOnce = append(pickOnce, it)
			}
		}
	}
	name := bundle.agent
	var hook *botItem
	var plain []byte
	hookURL := ""
	defer func() { wipe(plain) }()
	for _, it := range pick {
		if it.key != webhookItem {
			continue
		}
		hook, hookURL = it, it.url
		if it.paste { // typed by the user on the approval screen, covered by their proof
			u, k, err := c.pastedWebhook(approvals.InputsFrom(ctx))
			if err != nil {
				c.discardBotRequest(bundle.approvalID)
				return nil, err
			}
			hookURL, plain = u, []byte(k)
			continue
		}
		var err error
		if plain, err = c.botReqs.open(it.sealed, sealAD(name, it.url)); err != nil {
			return nil, err
		}
	}
	var granted []resolved
	err := c.Vault.Update(func(d *vault.Data) error {
		a := findAgent(d, name)
		if hook != nil {
			cur := ""
			if a != nil {
				cur = a.URL
			}
			if cur != hook.current { // what the user saw ("new" or "replaces X") is no longer true
				return errors.New(name + "'s webhook changed since this request was made, so nothing was changed. Ask the Bot to send its request again.")
			}
			if a == nil {
				a = &vault.Agent{Name: name, Default: len(d.Agents) == 0}
				d.Agents = append(d.Agents, a)
			}
			a.URL, a.Key = hookURL, string(plain)
			if hook.subsSet {
				a.Subscriptions = hook.subs
			}
		}
		for _, it := range pick {
			if it.key == webhookItem {
				continue
			}
			if a == nil {
				return errors.New(name + " isn't connected to Rubi (its webhook was left out), so it can't be given accounts. Nothing was changed.")
			}
			acc, err := accountOf(d, it.plugin, it.account)
			if err != nil {
				return err
			}
			found := false
			for _, n := range acc.Agents {
				found = found || strings.EqualFold(n, a.Name)
			}
			if !found {
				acc.Agents = append(acc.Agents, a.Name)
			}
			delete(acc.TempAgents, a.Name)
			m, _ := c.Store.Get(it.plugin)
			granted = append(granted, resolved{plugin: it.plugin, name: m.Name, account: acc.ID, label: acc.Label, assigned: true})
		}
		return nil
	})
	c.discardBotRequest(bundle.approvalID) // applied or not, the sealed key has done its job
	if err != nil {
		return nil, err
	}
	var done []string
	for _, it := range pick {
		done = append(done, it.label)
	}
	onceReceipt, onceErr := c.applyOnce(bundle, pickOnce, duration)
	if bundle.once != nil && onceErr == nil && onceReceipt != "" {
		var names []string
		for _, it := range pickOnce {
			names = append(names, it.res.title())
		}
		c.Audit.Record("bot.request.once", audit.Fields{"approval_id": bundle.approvalID, "agent": name,
			"once_accounts": strings.Join(names, ", "), "duration": duration, "task": bundle.once.task})
	}
	for _, it := range pickOnce {
		done = append(done, it.label)
	}
	c.Audit.Record("bot.request.applied", audit.Fields{"approval_id": bundle.approvalID, "agent": name,
		"webhook": hook != nil, "replaced": hook != nil && hook.replaces, "accounts": len(granted)})
	for _, g := range granted {
		c.sendAssignedCodes(g.plugin, g.account, []string{name})
	}
	res := map[string]any{"applied": done}
	receipt := strings.Join(done, "; ") + "."
	if hook != nil { // the same test the panel runs after a paste: does the routine take Rubi's events?
		t := c.TestWebhookWait(name, botWebhookTestWait)
		res["webhook_test"] = t
		switch {
		case t.Delivered:
			receipt += " Test event delivered: " + name + " has been told."
		case t.Problem != "":
			receipt += " Test: " + t.Problem
		}
	}
	if onceReceipt != "" {
		receipt += " " + onceReceipt
	}
	if onceErr != nil {
		res["access_failed"] = onceErr.Error()
		receipt += " The temporary access was not given: " + onceErr.Error()
	}
	res["receipt"] = receipt
	return res, nil
}

// applyOnce gives the one-off part of a combined card with the duration the user chose: exactly what the access
// approval does (grantOnce). The accounts the Bot is assigned to already get their task code when the card is
// approved, even if the user unticked every one-off item (they need no approval, and the task is the Bot's whole
// reason to ask); unticked accounts get nothing, and then the duration chosen does not apply (this task only).
func (c *Core) applyOnce(bundle *botBundle, picked []*botItem, duration string) (receipt string, err error) {
	if bundle.once == nil || (len(picked) == 0 && len(bundle.once.granted) == 0) {
		return "", nil
	}
	if _, ok := map[string]bool{"allow": true, "day": true, "week": true, "always": true}[duration]; !ok || len(picked) == 0 {
		duration = "allow" // anything else, or no account to keep, is the shortest: this task
	}
	var hook *vault.Agent
	_ = c.Vault.View(func(d *vault.Data) error {
		if a := findAgent(d, bundle.agent); a != nil {
			cp := *a
			hook = &cp
		}
		return nil
	})
	if hook == nil {
		return "", errors.New(bundle.agent + " isn't connected to Rubi (its webhook was left out)")
	}
	var allowed []resolved
	for _, it := range picked {
		allowed = append(allowed, it.res)
	}
	out, err := c.grantOnce(*hook, bundle.once.granted, allowed, bundle.once.task, bundle.once.d, duration)
	if err != nil {
		return "", err
	}
	r, _ := out["receipt"].(string)
	return r, nil
}

// botRequestFinished drops a Bot request once its approval ends (denied, expired, cancelled, done).
func (c *Core) botRequestFinished(s approvals.Snapshot) {
	if s.Kind == botRequestKind && s.State != approvals.Pending && s.State != approvals.Executing {
		c.discardBotRequest(s.ID)
	}
}

// expireBotRequest runs when a request's time is up: the approval expires and the key is wiped, even if
// nobody looks at it again.
func (c *Core) expireBotRequest(id string) {
	if s, err := c.Approvals.Get(id); err == nil && (s.State == approvals.Pending || s.State == approvals.Executing) {
		return // not yet (a clock adjustment): the approval's own expiry still applies when it is next read
	}
	c.discardBotRequest(id)
}

// discardBotRequest forgets a request and wipes its sealed key.
func (c *Core) discardBotRequest(id string) {
	b := &c.botReqs
	b.mu.Lock()
	bundle := b.byID[id]
	if bundle != nil {
		delete(b.byID, id)
		if b.byAgent[strings.ToLower(bundle.agent)] == bundle {
			delete(b.byAgent, strings.ToLower(bundle.agent))
		}
		if bundle.timer != nil {
			bundle.timer.Stop()
		}
		for _, it := range bundle.items {
			wipe(it.sealed)
			it.sealed = nil
		}
	}
	b.mu.Unlock()
	if bundle != nil {
		c.Audit.Record("bot.request.discarded", audit.Fields{"approval_id": id, "agent": bundle.agent})
	}
}

// discardBotRequests wipes every waiting request (Rubi locks).
func (c *Core) discardBotRequests() {
	b := &c.botReqs
	b.mu.Lock()
	var ids []string
	for id := range b.byID {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	for _, id := range ids {
		c.discardBotRequest(id)
	}
}

// pendingBotRequest reports the waiting request of a Bot, for tests and status ("" = none).
func (c *Core) pendingBotRequest(agent string) string {
	b := &c.botReqs
	b.mu.Lock()
	defer b.mu.Unlock()
	if x := b.byAgent[strings.ToLower(strings.TrimSpace(agent))]; x != nil {
		return x.approvalID
	}
	return ""
}

// allItems is what the card lists: the webhook, the permanent accounts, then the one-off accounts.
func (b *botBundle) hasOnceItems() bool { return b.once != nil && len(b.once.items) > 0 }

// labelOf is an item's label on the card. When the card holds one-off items, every other item says it is for
// good, so that the two kinds can't be taken for each other.
func (b *botBundle) labelOf(it *botItem) string {
	if b.hasOnceItems() && !it.once {
		return it.label + " for good"
	}
	return it.label
}

func (b *botBundle) allItems() []*botItem {
	all := append([]*botItem{}, b.items...)
	if b.once != nil {
		all = append(all, b.once.items...)
	}
	return all
}

// onceItem is one account of a Bot's one-off access, as an item of the combined card. It says in plain words that
// its duration is the user's choice, so that it can't be taken for the permanent ones next to it.
func (c *Core) onceItem(agent, task string, r resolved) *botItem {
	m, _ := c.Store.Get(r.plugin)
	p := map[string]any{"bot": agent, "plugin": r.name, "account": r.label, "task": task,
		"effect": agent + " may use this account for the time you choose above (this task, a day, a week, or always), for the task. Actions that need your approval still do."}
	var can []string
	for _, act := range m.Actions {
		can = append(can, act.Title+" ("+string(act.DefaultLevel)+" approval)")
	}
	for _, t := range m.Tools {
		can = append(can, t.Name)
	}
	if len(can) > 0 {
		p["can"] = strings.Join(can, "; ")
	}
	if len(m.Egress) > 0 {
		p["connects_to"] = strings.Join(m.Egress, ", ")
	}
	return &botItem{key: "once:" + r.key(), label: "Let " + agent + " use " + r.title() + " for the time you choose",
		preview: p, plugin: r.plugin, account: r.account, once: true, res: r}
}

// canJoinBotRequest: the Bot has a request waiting that this one-off access can join (it holds no other one-off
// access, or one for the same task).
func (c *Core) canJoinBotRequest(agent string, once *onceAsk) bool {
	b := &c.botReqs
	b.mu.Lock()
	defer b.mu.Unlock()
	x := b.byAgent[strings.ToLower(agent)]
	return x != nil && len(x.items) > 0 && (x.once == nil || x.once.task == once.task)
}

// rememberPendingOnce keeps a one-off access approval that waits on its own, for a later request of the same Bot to
// join. Other Bots' asks are never touched: the key is the Bot.
func (c *Core) rememberPendingOnce(agent string, out map[string]any, once *onceAsk) {
	id, _ := out["approval_id"].(string)
	if id == "" {
		return
	}
	b := &c.botReqs
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cipher()
	b.onceBy[strings.ToLower(agent)] = &pendingOnce{approvalID: id, once: once}
}

// peekPendingOnce returns the Bot's waiting one-off access approval, if it still waits.
func (c *Core) peekPendingOnce(agent string) *pendingOnce {
	b := &c.botReqs
	b.mu.Lock()
	p := b.onceBy[strings.ToLower(agent)]
	b.mu.Unlock()
	if p == nil {
		return nil
	}
	if s, err := c.Approvals.Get(p.approvalID); err != nil || s.State != approvals.Pending {
		b.mu.Lock()
		delete(b.onceBy, strings.ToLower(agent))
		b.mu.Unlock()
		return nil
	}
	return p
}

// combineOnce joins two one-off access asks of one Bot. They must be for the same task: one code is made for a task.
func combineOnce(a, b *onceAsk) (*onceAsk, error) {
	if a == nil {
		return b, nil
	}
	if b == nil {
		return a, nil
	}
	if a.task != b.task {
		return nil, fmt.Errorf("two one-off access requests of yours, for different tasks (%q and %q), can't share one approval: one code is made for one task", a.task, b.task)
	}
	out := &onceAsk{task: a.task, d: max(a.d, b.d), granted: append([]resolved{}, a.granted...), items: append([]*botItem{}, a.items...)}
	for _, it := range b.items {
		if !out.has(it.key) {
			out.items = append(out.items, it)
		}
	}
	for _, g := range b.granted {
		dup := false
		for _, x := range out.granted {
			dup = dup || x.key() == g.key()
		}
		if !dup {
			out.granted = append(out.granted, g)
		}
	}
	return out, nil
}
