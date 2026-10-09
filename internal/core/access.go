package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	issued map[string]time.Time   // agent|plugin|account -> last issue
}

type accessGrant struct {
	agent, plugin, account string
	expires                time.Time
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
			return strings.EqualFold(gr.agent, agent) && gr.plugin == plugin && gr.account == account
		}
	}
	return false
}

// RequestAccess is a Bot asking to use one plugin account for a task, for a while.
func (c *Core) RequestAccess(ctx context.Context, agent, plugin, account, task string, minutes int) (map[string]any, error) {
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
	m, ok := c.Store.Get(plugin)
	if !ok {
		return nil, fmt.Errorf("plugin %q is not installed", plugin)
	}
	var hook *vault.Agent
	var acctID, label string
	assigned := false
	if err := c.Vault.View(func(dd *vault.Data) error {
		a := findAgent(dd, agent)
		if a == nil {
			return fmt.Errorf("no agent %q (agents: %s)", agent, c.agentNames())
		}
		cp := *a
		hook = &cp
		acc, err := accountOf(dd, plugin, account)
		if err != nil {
			return err
		}
		acctID, label = acc.ID, acc.Label
		if len(acc.Agents) == 0 {
			assigned = a == defaultAgent(dd) // nobody assigned: the administrator
		}
		for _, n := range acc.Agents {
			assigned = assigned || strings.EqualFold(n, a.Name)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	what := m.Name + " (" + label + ")"
	if assigned {
		if err := c.issueAccess(*hook, m, acctID, label, task, d); err != nil {
			return nil, err
		}
		return map[string]any{"status": "sent", "message": "The code is on its way to your webhook. The run it starts continues with your task; tell the user it's underway."}, nil
	}
	return c.Approvals.Submit(ctx, approvals.Request{Integration: "rubi", Kind: "rubi.access",
		Summary: fmt.Sprintf("Let %s use %s for %s", hook.Name, what, d),
		Preview: map[string]any{"bot": hook.Name, "plugin": m.Name, "account": label, "task": task, "for": d.String(),
			"note": hook.Name + " isn't assigned to this account. Approving lets it in once, for this long."},
		Options: []approvals.Option{{Key: "allow", Label: "Allow"}},
		Execute: func(context.Context, string) (any, error) {
			if err := c.issueAccess(*hook, m, acctID, label, task, d); err != nil {
				return nil, err
			}
			return map[string]any{"granted": true, "for": d.String()}, nil
		}})
}

func (c *Core) issueAccess(hook vault.Agent, m plugins.Manifest, acct, label, task string, d time.Duration) error {
	g := &c.access
	key := strings.ToLower(hook.Name) + "|" + m.ID + "|" + acct
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
	g.grants[code] = accessGrant{agent: hook.Name, plugin: m.ID, account: acct, expires: time.Now().Add(d)}
	g.issued[key] = time.Now()
	g.mu.Unlock()
	c.Audit.Record("access.granted", audit.Fields{"agent": hook.Name, "plugin": m.ID, "account": label, "task": task, "minutes": int(d.Minutes())})
	// Straight to the Bot's webhook; never kept in the event list.
	ev := events.Event{ID: "evt_access_" + randomID()[:8], Integration: "rubi", Type: "access.granted", CreatedAt: time.Now().UTC(),
		Data: map[string]any{"plugin": m.ID, "account": label, "task": task, "expires_at": time.Now().Add(d).UTC(),
			"access_code": code,
			"next_step":   "Continue your task: " + task + ". Pass rubi_agent=\"" + hook.Name + "\" and rubi_access=<data.access_code> to every " + m.Name + " tool. The code works only for you, for " + label + ", until expires_at; don't share it."}}
	go c.deliverTo(hook, ev)
	return nil
}

// SetAccountAgents asks the user to choose which Bots may use one plugin account.
func (c *Core) SetAccountAgents(ctx context.Context, plugin, account string, agents []string) (string, error) {
	var label string
	var clean []string
	if err := c.Vault.View(func(d *vault.Data) error {
		a, err := accountOf(d, plugin, account)
		if err != nil {
			return err
		}
		label = a.Label
		for _, n := range agents {
			x := findAgent(d, n)
			if x == nil {
				return fmt.Errorf("no agent %q", n)
			}
			clean = append(clean, x.Name)
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
	return c.RequestChange(ctx, "Bots for "+m.Name+" ("+label+"): "+who, map[string]any{"plugin": m.Name, "account": label, "bots": who,
		"effect": "These Bots get access codes for this account when they ask; any other Bot needs your approval each time."},
		func(d *vault.Data) error {
			a, err := accountOf(d, plugin, account)
			if err != nil {
				return err
			}
			a.Agents = clean
			return nil
		}, nil)
}
