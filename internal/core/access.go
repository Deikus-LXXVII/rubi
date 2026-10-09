package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/audit"
	"github.com/Deikus-LXXVII/rubi/internal/events"
	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// Access codes. When several Bots share Rubi, each plugin account (a mailbox, a Telegram account, a home)
// has the Bots the user assigned to it; with none assigned, the administrator. A Bot that wants to use an
// account asks for access with its name, its task and how long it needs (at most 2 hours). An assigned
// Bot gets a code at once; any other Bot only after the user approves. The code always travels through the
// Bot's own webhook, which only that Bot receives, and the run it starts carries on with the task. Every
// plugin call then names the Bot and its code, and is recorded with the Bot's name.
//
// With a single Bot there is nobody to tell apart, and plugins work without codes.

const (
	maxAccess     = 2 * time.Hour
	defaultAccess = time.Hour
	accessEvery   = 2 * time.Minute // one code per Bot, plugin and account this often (each costs a wake)

	// AccessAgentArg and AccessCodeArg are the arguments every plugin tool takes when codes are on.
	AccessAgentArg = "rubi_agent"
	AccessCodeArg  = "rubi_access"
)

type accessGrants struct {
	mu     sync.Mutex
	grants map[string]accessGrant // by code
	issued map[string]time.Time   // agent|resources -> last issue
}

// accessGrant is one code: one Bot, the plugin accounts it may use (plugin|account), until expires.
type accessGrant struct {
	agent     string
	resources map[string]bool
	expires   time.Time
}

// AccessResource names one plugin account a Bot asks for ("" account: the plugin's default one).
type AccessResource struct {
	Plugin  string `json:"plugin"`
	Account string `json:"account,omitempty"`
}

// AccessRequired reports whether plugin calls need access codes (two or more Bots connected).
func (c *Core) AccessRequired() bool {
	n := 0
	_ = c.Vault.View(func(d *vault.Data) error { n = len(d.Agents); return nil })
	return n >= 2
}

// checkAccess takes the access arguments out of a plugin call and, when codes are on, checks them. It
// returns the arguments to pass on, or a reply for the agent when access is missing.
func (c *Core) checkAccess(m plugins.Manifest, tool string, args json.RawMessage) (json.RawMessage, map[string]any, error) {
	var in map[string]any
	if len(args) > 0 && json.Unmarshal(args, &in) != nil {
		return args, nil, nil // not an object: the plugin reports it
	}
	agent, _ := in[AccessAgentArg].(string)
	code, _ := in[AccessCodeArg].(string)
	if in != nil {
		delete(in, AccessAgentArg)
		delete(in, AccessCodeArg)
		args, _ = json.Marshal(in)
	}
	if !c.AccessRequired() {
		return args, nil, nil
	}
	ref, _ := in["account"].(string)
	var acct, label string
	if err := c.Vault.View(func(d *vault.Data) error {
		a, err := accountOf(d, m.ID, ref)
		if err != nil {
			return err
		}
		acct, label = a.ID, a.Label
		return nil
	}); err != nil {
		return nil, nil, err
	}
	if !c.validAccess(agent, code, m.ID, acct) {
		return nil, map[string]any{"status": "access_needed",
			"message": "Several Bots share this Rubi, so using " + m.Name + " (" + label + ") needs an access code. Call rubi_access with your Bot name (agent), plugin \"" + m.ID + "\", account, your task and the minutes you need (up to 120). The code arrives at your webhook and starts a run of yours that carries on with the task; pass rubi_agent and rubi_access to every " + m.Name + " tool there."}, nil
	}
	c.Audit.Record("plugin.tool", audit.Fields{"agent": agent, "tool": tool, "plugin": m.ID, "account": label})
	return args, nil, nil
}

func (c *Core) validAccess(agent, code, plugin, account string) bool {
	if agent == "" || code == "" {
		return false
	}
	g := &c.access
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, gr := range g.grants {
		if time.Now().After(gr.expires) {
			delete(g.grants, k)
		}
	}
	for k, gr := range g.grants {
		if subtle.ConstantTimeCompare([]byte(k), []byte(code)) == 1 {
			return strings.EqualFold(gr.agent, agent) && gr.resources[plugin+"|"+account]
		}
	}
	return false
}

// resolved is one requested plugin account, checked.
type resolved struct {
	plugin, name, account, label string
	assigned                     bool
}

func (r resolved) key() string   { return r.plugin + "|" + r.account }
func (r resolved) title() string { return r.name + " (" + r.label + ")" }

// RequestAccess is a Bot asking to use plugin accounts for a task, for a while: one request for any number
// of accounts, even of different plugins. Those it is assigned to are granted at once; for the others the
// user gets one approval with every account listed, and ticks the ones to allow. Either way the Bot gets
// one code, for everything granted, through its webhook.
func (c *Core) RequestAccess(ctx context.Context, agent string, want []AccessResource, task string, minutes int) (map[string]any, error) {
	if c.State() != Unlocked {
		return nil, errors.New("Rubi is locked")
	}
	if !c.AccessRequired() {
		return map[string]any{"status": "not_needed", "message": "Only one Bot uses this Rubi: plugins work without access codes."}, nil
	}
	task = strings.TrimSpace(task)
	if task == "" || len(task) > 500 {
		return nil, errors.New("describe your task (up to 500 characters); it is shown to the user and kept in the log")
	}
	d := time.Duration(minutes) * time.Minute
	if minutes <= 0 {
		d = defaultAccess
	}
	if d > maxAccess {
		return nil, errors.New("access lasts at most 120 minutes; ask again when you need more")
	}
	if len(want) == 0 || len(want) > 50 {
		return nil, errors.New("name between 1 and 50 plugin accounts")
	}
	var hook *vault.Agent
	var items []resolved
	seen := map[string]bool{}
	if err := c.Vault.View(func(dd *vault.Data) error {
		a := findAgent(dd, agent)
		if a == nil {
			return fmt.Errorf("no agent %q (agents: %s)", agent, c.agentNames())
		}
		cp := *a
		hook = &cp
		for _, w := range want {
			m, ok := c.Store.Get(w.Plugin)
			if !ok {
				return fmt.Errorf("plugin %q is not installed", w.Plugin)
			}
			acc, err := accountOf(dd, w.Plugin, w.Account)
			if err != nil {
				return fmt.Errorf("%s: %w", m.Name, err)
			}
			r := resolved{plugin: m.ID, name: m.Name, account: acc.ID, label: acc.Label}
			if seen[r.key()] {
				continue
			}
			seen[r.key()] = true
			if len(acc.Agents) == 0 {
				r.assigned = a == defaultAgent(dd) // nobody assigned: the administrator
			}
			r.assigned = r.assigned || acc.AgentAllowed(a.Name, time.Now())
			items = append(items, r)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	var granted, ask []resolved
	for _, r := range items {
		if r.assigned {
			granted = append(granted, r)
		} else {
			ask = append(ask, r)
		}
	}
	if len(ask) == 0 {
		if err := c.issueAccess(*hook, granted, task, d); err != nil {
			return nil, err
		}
		return map[string]any{"status": "sent", "message": "The code is on its way to your webhook. The run it starts continues with your task; tell the user it's underway."}, nil
	}
	preview := map[string]any{"bot": hook.Name, "task": task, "asks_for": humanDuration(d),
		"note": hook.Name + " isn't assigned to these accounts. Choose how long to let it in, and (when there are several) which accounts."}
	if len(granted) > 0 {
		var names []string
		for _, r := range granted {
			names = append(names, r.title())
		}
		preview["already_allowed"] = strings.Join(names, ", ")
	}
	// The user decides how long: this task only, a day, a week, or for good. Longer than the task makes
	// the Bot assigned to those accounts for that long (later requests are granted without asking).
	req := approvals.Request{Integration: "rubi", Kind: "rubi.access", Preview: preview,
		Question: "How long may " + hook.Name + " use them?",
		Options: []approvals.Option{
			{Key: "allow", Label: "Allow for this task", Meaning: humanDuration(d)},
			{Key: "day", Label: "Allow for a day", Meaning: "it won't need to ask again until tomorrow"},
			{Key: "week", Label: "Allow for a week"},
			{Key: "always", Label: "Always allow", Meaning: "assigns it to these accounts; you can undo it in Settings"},
		},
		Execute: func(_ context.Context, option string) (any, error) {
			key, items := approvals.SplitOption(option)
			chosen := map[string]bool{}
			for _, k := range items {
				chosen[k] = true
			}
			all := append([]resolved{}, granted...)
			var allowed []resolved
			for _, r := range ask {
				if chosen[r.key()] || items == nil {
					all = append(all, r)
					allowed = append(allowed, r)
				}
			}
			if err := c.rememberAccess(hook.Name, allowed, key); err != nil {
				return nil, err
			}
			if err := c.issueAccess(*hook, all, task, d); err != nil {
				return nil, err
			}
			// What the user allowed, in their words, for the receipt (not the duration the Bot asked for).
			var names []string
			for _, r := range allowed {
				names = append(names, r.title())
			}
			how := map[string]string{"day": "for a day", "week": "for a week", "always": "from now on"}[key]
			if how == "" {
				how = "for " + humanDuration(d)
			}
			return map[string]any{"granted": len(all), "for": d.String(), "kept": key,
				"receipt": hook.Name + " may use " + strings.Join(names, ", ") + " " + how + "."}, nil
		}}
	if len(ask) == 1 {
		req.Summary = fmt.Sprintf("Let %s use %s", hook.Name, ask[0].title())
	} else {
		req.Summary = fmt.Sprintf("Let %s use %d accounts", hook.Name, len(ask))
		for _, r := range ask {
			req.Items = append(req.Items, approvals.Item{Key: r.key(), Label: r.title()})
		}
	}
	return c.Approvals.Submit(ctx, req)
}

// rememberAccess keeps a longer permission: "day" and "week" assign the Bot to the accounts for that
// long, "always" for good; "allow" keeps nothing beyond this task's code.
func (c *Core) rememberAccess(agent string, accounts []resolved, how string) error {
	var until time.Time
	switch how {
	case "day":
		until = time.Now().Add(24 * time.Hour)
	case "week":
		until = time.Now().Add(7 * 24 * time.Hour)
	case "always":
	default:
		return nil
	}
	err := c.Vault.Update(func(d *vault.Data) error {
		for _, r := range accounts {
			acc, err := accountOf(d, r.plugin, r.account)
			if err != nil {
				continue
			}
			if how == "always" {
				found := false
				for _, n := range acc.Agents {
					found = found || strings.EqualFold(n, agent)
				}
				if !found {
					acc.Agents = append(acc.Agents, agent)
				}
				delete(acc.TempAgents, agent)
				continue
			}
			if acc.TempAgents == nil {
				acc.TempAgents = map[string]time.Time{}
			}
			acc.TempAgents[agent] = until.UTC()
		}
		return nil
	})
	if err == nil {
		c.Audit.Record("access.kept", audit.Fields{"agent": agent, "how": how, "accounts": len(accounts)})
	}
	return err
}

// revokeAccess ends the codes a Bot holds for one account (when the user takes the account away from it).
func (c *Core) revokeAccess(agent, plugin, account string) {
	g := &c.access
	g.mu.Lock()
	defer g.mu.Unlock()
	for code, gr := range g.grants {
		if strings.EqualFold(gr.agent, agent) && gr.resources[plugin+"|"+account] {
			delete(g.grants, code)
		}
	}
}

// issueAccess makes one code for all the granted accounts and sends it to the Bot's webhook.
func (c *Core) issueAccess(hook vault.Agent, granted []resolved, task string, d time.Duration) error {
	if len(granted) == 0 {
		return errors.New("nothing was allowed")
	}
	res := map[string]bool{}
	var keys, names []string
	for _, r := range granted {
		res[r.key()] = true
		keys = append(keys, r.key())
		names = append(names, r.title())
	}
	sort.Strings(keys)
	g := &c.access
	key := strings.ToLower(hook.Name) + "|" + strings.Join(keys, ",")
	g.mu.Lock()
	if g.grants == nil {
		g.grants, g.issued = map[string]accessGrant{}, map[string]time.Time{}
	}
	if last, ok := g.issued[key]; ok && time.Since(last) < accessEvery {
		g.mu.Unlock()
		return errors.New("a code for this was sent to your webhook a moment ago; use that one")
	}
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	code := base64.RawURLEncoding.EncodeToString(b)
	g.grants[code] = accessGrant{agent: hook.Name, resources: res, expires: time.Now().Add(d)}
	g.issued[key] = time.Now()
	g.mu.Unlock()
	what := strings.Join(names, ", ")
	c.Audit.Record("access.granted", audit.Fields{"agent": hook.Name, "accounts": what, "task": task, "minutes": int(d.Minutes())})
	// Straight to the Bot's webhook; never kept in the event list.
	ev := events.Event{ID: "evt_access_" + randomID()[:8], Integration: "rubi", Type: "access.granted", CreatedAt: time.Now().UTC(),
		Data: map[string]any{"accounts": names, "task": task, "expires_at": time.Now().Add(d).UTC(), "access_code": code,
			"next_step": "Continue your task: " + task + ". Pass rubi_agent=\"" + hook.Name + "\" and rubi_access=<data.access_code> to the tools of these accounts: " + what + ". The code works only for you and only for them, until expires_at; don't share it."}}
	go c.deliverTo(hook, ev)
	return nil
}

// SetAccountAgents asks the user to choose which Bots may use one plugin account: agents for good, and of
// those let in for a while, the ones to keep (keepTemp). Anyone dropped loses its codes for the account.
func (c *Core) SetAccountAgents(ctx context.Context, plugin, account string, agents, keepTemp []string) (string, error) {
	var label, acctID string
	var clean []string
	keep := map[string]bool{}
	if err := c.Vault.View(func(d *vault.Data) error {
		a, err := accountOf(d, plugin, account)
		if err != nil {
			return err
		}
		label, acctID = a.Label, a.ID
		for _, n := range agents {
			x := findAgent(d, n)
			if x == nil {
				return fmt.Errorf("no agent %q", n)
			}
			clean = append(clean, x.Name)
		}
		for _, n := range keepTemp {
			keep[strings.ToLower(n)] = true
		}
		return nil
	}); err != nil {
		return "", err
	}
	m, _ := c.Store.Get(plugin)
	who := strings.Join(clean, ", ")
	if who == "" {
		who = "the administrator only"
	}
	var dropped []string
	return c.RequestChange(ctx, "Bots for "+m.Name+" ("+label+"): "+who, map[string]any{"plugin": m.Name, "account": label, "bots": who,
		"effect": "These Bots get access codes for this account when they ask; any other Bot needs your approval each time."},
		func(d *vault.Data) error {
			a, err := accountOf(d, plugin, account)
			if err != nil {
				return err
			}
			now := map[string]bool{}
			for _, n := range clean {
				now[strings.ToLower(n)] = true
			}
			for _, n := range a.Agents {
				if !now[strings.ToLower(n)] {
					dropped = append(dropped, n)
				}
			}
			for n := range a.TempAgents {
				if !keep[strings.ToLower(n)] && !now[strings.ToLower(n)] {
					delete(a.TempAgents, n)
					dropped = append(dropped, n)
				} else if now[strings.ToLower(n)] {
					delete(a.TempAgents, n) // now for good
				}
			}
			a.Agents = clean
			return nil
		}, func() {
			for _, n := range dropped {
				c.revokeAccess(n, plugin, acctID)
			}
		})
}

// PluginAccounts lists the ids of a plugin's connected accounts.
func (c *Core) PluginAccounts(plugin string) []string {
	var out []string
	_ = c.Vault.View(func(d *vault.Data) error {
		if i := d.Integrations[plugin]; i != nil {
			for _, a := range i.Accounts {
				out = append(out, a.ID)
			}
		}
		return nil
	})
	return out
}

// humanDuration says a duration the way a person would ("2 hours", "45 minutes").
func humanDuration(d time.Duration) string {
	m := int(d.Round(time.Minute).Minutes())
	switch {
	case m%60 == 0 && m >= 60:
		if m == 60 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", m/60)
	case m > 60:
		return fmt.Sprintf("%d h %d min", m/60, m%60)
	case m == 1:
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", m)
}
