package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/Deikus-LXXVII/rubi/internal/events"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// Several Grok Bots can share one Rubi (they share one computer, and the same MCP server). Each Bot that
// wants to be woken registers as an agent with its own routine webhook. Rubi routes:
//   - an approval's outcome to the agent that asked to be woken (rubi_continue_after);
//   - other events to every agent subscribed to their source (a plugin, or "rubi");
//   - events nobody subscribed to, to the default agent.

// AgentInfo is an agent as the agent and the panel see it (never the key).
type AgentInfo struct {
	Name          string   `json:"name"`
	Host          string   `json:"host"`
	Default       bool     `json:"default"`
	Subscriptions []string `json:"subscriptions"`
}

const maxAgentName = 40

// CleanAgentName validates an agent's display name (its Bot's name).
func CleanAgentName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > maxAgentName {
		return "", errors.New("the agent name must be 1-40 characters (use your Bot's name)")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", errors.New("the agent name contains invalid characters")
		}
	}
	return name, nil
}

func findAgent(d *vault.Data, name string) *vault.Agent {
	for _, a := range d.Agents {
		if strings.EqualFold(a.Name, strings.TrimSpace(name)) {
			return a
		}
	}
	return nil
}

func defaultAgent(d *vault.Data) *vault.Agent {
	for _, a := range d.Agents {
		if a.Default {
			return a
		}
	}
	if len(d.Agents) > 0 {
		return d.Agents[0]
	}
	return nil
}

// Agents lists the registered agents.
func (c *Core) Agents() []AgentInfo {
	out := []AgentInfo{}
	_ = c.Vault.View(func(d *vault.Data) error {
		def := defaultAgent(d)
		for _, a := range d.Agents {
			subs := append([]string{}, a.Subscriptions...)
			out = append(out, AgentInfo{Name: a.Name, Host: redactURL(a.URL), Default: a == def, Subscriptions: subs})
		}
		return nil
	})
	return out
}

// agentNames is for error messages.
func (c *Core) agentNames() string {
	var names []string
	for _, a := range c.Agents() {
		names = append(names, fmt.Sprintf("%q", a.Name))
	}
	if len(names) == 0 {
		return "none yet"
	}
	return strings.Join(names, ", ")
}

// HasAgent reports whether name is a registered agent.
func (c *Core) HasAgent(name string) bool {
	found := false
	_ = c.Vault.View(func(d *vault.Data) error {
		found = findAgent(d, name) != nil
		return nil
	})
	return found
}

// webhookAgent returns the registered name of the Bot called name, if it is connected with a webhook
// (registered with the user's passkey).
func (c *Core) webhookAgent(name string) (string, bool) {
	var out string
	_ = c.Vault.View(func(d *vault.Data) error {
		if a := findAgent(d, name); a != nil && a.URL != "" {
			out = a.Name
		}
		return nil
	})
	return out, out != ""
}

// oneLineName shortens a name a plugin or Bot passed, for messages and the audit log.
func oneLineName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 64 {
		s = string(r[:64]) + "…"
	}
	return s
}

// AddAgent requests approval to register an agent's webhook (or replace the webhook of an agent with the
// same name). The first agent becomes the default.
// subs, when not nil, sets what the Bot hears about (sources: "rubi" or plugin ids), as part of the same
// approval.
func (c *Core) AddAgent(ctx context.Context, name, rawURL, key string, subs []string) (string, error) {
	name, err := CleanAgentName(name)
	if err != nil {
		return "", err
	}
	if subs != nil {
		if subs, err = c.cleanSources(subs); err != nil {
			return "", err
		}
	}
	if err := validWebhookURL(rawURL); err != nil {
		return "", err
	}
	if key, err = cleanWebhookKey(key); err != nil {
		return "", err
	}
	// The end of the address identifies the routine, so the user can tell their own from a look-alike
	// on the same host; replacing an existing Bot's webhook says so.
	preview := map[string]any{"agent": name, "webhook": webhookLabel(rawURL)}
	if !isRoutineWebhook(rawURL) || !strings.HasPrefix(key, routineKeyPrefix) {
		preview["warning"] = "This is not a Grok Bot routine webhook (" + routineHost + ", key " + routineKeyPrefix +
			"…). Approve only if you set up this receiver yourself."
	}
	var current string
	_ = c.Vault.View(func(d *vault.Data) error {
		if a := findAgent(d, name); a != nil && a.URL != "" && a.URL != rawURL {
			current = webhookLabel(a.URL)
		}
		return nil
	})
	if current != "" {
		preview["replaces"] = current
	}
	if subs != nil {
		preview["notifies_about"] = c.describeSources(subs)
	}
	return c.RequestChange(ctx, "Let Rubi wake "+name, preview,
		func(d *vault.Data) error {
			a := findAgent(d, name)
			if a == nil {
				a = &vault.Agent{Name: name, Default: len(d.Agents) == 0}
				d.Agents = append(d.Agents, a)
			}
			a.URL, a.Key = rawURL, key
			if subs != nil {
				a.Subscriptions = subs
			}
			return nil
		}, nil)
}

// RemoveAgent requests approval to forget an agent. Its plugins fall back to the default agent.
func (c *Core) RemoveAgent(ctx context.Context, name string) (string, error) {
	if !c.HasAgent(name) {
		return "", fmt.Errorf("no agent %q (agents: %s)", name, c.agentNames())
	}
	return c.RequestChange(ctx, "Stop waking "+name, map[string]any{"agent": name,
		"effect": "Rubi won't send events to this Bot any more."},
		func(d *vault.Data) error {
			a := findAgent(d, name)
			var kept []*vault.Agent
			for _, x := range d.Agents {
				if x != a {
					kept = append(kept, x)
				}
			}
			if a != nil && a.Default && len(kept) > 0 {
				kept[0].Default = true
			}
			d.Agents = kept
			// It no longer has any account assigned (a later Bot with the same name starts with none).
			for _, i := range d.Integrations {
				for _, acc := range i.Accounts {
					var left []string
					for _, n := range acc.Agents {
						if a == nil || !strings.EqualFold(n, a.Name) {
							left = append(left, n)
						}
					}
					acc.Agents = left
				}
			}
			return nil
		}, nil)
}

// SetDefaultAgent requests approval to make an agent Rubi's administrator: the Bot responsible for Rubi,
// which gets Rubi's own events and is the only one that reads the whole event list.
func (c *Core) SetDefaultAgent(ctx context.Context, name string) (string, error) {
	if !c.HasAgent(name) {
		return "", fmt.Errorf("no agent %q (agents: %s)", name, c.agentNames())
	}
	return c.RequestChange(ctx, "Make "+name+" Rubi's administrator", map[string]any{"agent": name,
		"effect": "Gets Rubi's own events (approvals, updates, problems) and is the only Bot that reads every event. Other Bots see only their own."},
		func(d *vault.Data) error {
			a := findAgent(d, name)
			for _, x := range d.Agents {
				x.Default = x == a
			}
			return nil
		}, nil)
}

// Sources lists what an agent can subscribe to: installed plugins, and "rubi" for Rubi's own events.
func (c *Core) Sources() []map[string]string {
	out := []map[string]string{{"id": "rubi", "name": "Rubi (updates, plugin problems)"}}
	for _, m := range c.Store.Installed() {
		out = append(out, map[string]string{"id": m.ID, "name": m.Name})
	}
	return out
}

// SetSubscriptions replaces which event sources an agent hears about. It needs no approval: it only
// distributes events among webhooks the user already approved, and the user can change it in settings.
func (c *Core) SetSubscriptions(agent string, sources []string) ([]string, error) {
	clean, err := c.cleanSources(sources)
	if err != nil {
		return nil, err
	}
	names := c.agentNames()
	err = c.Vault.Update(func(d *vault.Data) error {
		a := findAgent(d, agent)
		if a == nil {
			return fmt.Errorf("no agent %q (agents: %s)", agent, names)
		}
		a.Subscriptions = clean
		return nil
	})
	if err == nil {
		c.Audit.Record("agent.subscriptions", map[string]any{"agent": agent, "sources": clean})
	}
	return clean, err
}

// RequestSubscriptions is a Bot choosing what it hears about. Dropping sources takes effect at once;
// adding one gives that Bot the plugin's events (including their content), so the user approves it.
// It returns an approval id when one is needed.
func (c *Core) RequestSubscriptions(ctx context.Context, agent string, sources []string) (clean []string, approvalID string, err error) {
	if clean, err = c.cleanSources(sources); err != nil {
		return nil, "", err
	}
	var current map[string]bool
	var name string
	_ = c.Vault.View(func(d *vault.Data) error {
		if a := findAgent(d, agent); a != nil {
			name = a.Name
			current = map[string]bool{}
			for _, s := range a.Subscriptions {
				current[s] = true
			}
		}
		return nil
	})
	if current == nil {
		return nil, "", fmt.Errorf("no agent %q (agents: %s)", agent, c.agentNames())
	}
	var added []string
	for _, s := range clean {
		if !current[s] {
			added = append(added, s)
		}
	}
	if len(added) == 0 {
		_, err = c.SetSubscriptions(name, clean)
		return clean, "", err
	}
	id, err := c.RequestChange(ctx, "Let "+name+" hear about "+c.describeSources(added), map[string]any{"agent": name,
		"effect": "This Bot gets these events, with what they contain (for example who wrote and the subject), and is woken by them."},
		func(d *vault.Data) error {
			if a := findAgent(d, name); a != nil {
				a.Subscriptions = clean
			}
			return nil
		}, nil)
	return clean, id, err
}

func (c *Core) cleanSources(sources []string) ([]string, error) {
	valid := map[string]bool{}
	for _, s := range c.Sources() {
		valid[s["id"]] = true
	}
	clean := []string{}
	seen := map[string]bool{}
	for _, s := range sources {
		s = strings.TrimSpace(s)
		if !valid[s] {
			return nil, fmt.Errorf("unknown source %q (use \"rubi\" or an installed plugin id)", s)
		}
		if !seen[s] {
			seen[s] = true
			clean = append(clean, s)
		}
	}
	return clean, nil
}

func (c *Core) describeSources(subs []string) string {
	if len(subs) == 0 {
		return "Only results of what this Bot asks for"
	}
	names := map[string]string{}
	for _, s := range c.Sources() {
		names[s["id"]] = s["name"]
	}
	out := make([]string, len(subs))
	for i, s := range subs {
		out[i] = names[s]
	}
	return strings.Join(out, ", ")
}

// recipients are the agents an event goes to: its explicit target, else everyone subscribed to its
// source, else the default agent.
func (c *Core) recipients(target, source string) []vault.Agent {
	var out []vault.Agent
	_ = c.Vault.View(func(d *vault.Data) error {
		if target != "" {
			// Addressed: that Bot's webhook or no one.
			if a := findAgent(d, target); a != nil && a.URL != "" {
				out = append(out, *a)
			}
			return nil
		} else {
			for _, a := range d.Agents {
				for _, s := range a.Subscriptions {
					if s == source {
						out = append(out, *a)
						break
					}
				}
			}
		}
		// Rubi's own events (approval outcomes, updates, problems) go to the administrator when no one
		// else asked for them. A plugin's events no Bot subscribed to wake no one: they wait in the
		// event list, which the administrator reads in full.
		if len(out) == 0 && source == "rubi" {
			if a := defaultAgent(d); a != nil {
				out = append(out, *a)
			}
		}
		return nil
	})
	return out
}

// EventsFor is the event list as one agent may see it. The administrator sees every event not addressed
// to another Bot; another Bot sees the events addressed to it and those of the sources it subscribed to.
// An event addressed to a Bot is that Bot's alone. Before any Bot is connected
// (setup) the list is whole.
// Reading needs the Bot's current code (see agentauth.go).
func (c *Core) EventsFor(agent, code string, includeAcked bool) ([]events.Event, error) {
	if err := c.checkAgentCode(agent, code); err != nil {
		return nil, err
	}
	return c.eventsVisible(agent, includeAcked)
}

// WaitingFor counts the unreported events agent may see, without needing its code (a number reveals
// nothing; the agent then asks for its code).
func (c *Core) WaitingFor(agent string) int {
	list, _ := c.eventsVisible(agent, false)
	return len(list)
}

func (c *Core) eventsVisible(agent string, includeAcked bool) ([]events.Event, error) {
	all := c.Events.ListFor(agent, includeAcked)
	var admin bool
	var subs map[string]bool
	var known, none bool
	_ = c.Vault.View(func(d *vault.Data) error {
		none = len(d.Agents) == 0
		if a := findAgent(d, agent); a != nil {
			known = true
			admin = a == defaultAgent(d)
			subs = map[string]bool{}
			for _, s := range a.Subscriptions {
				subs[s] = true
			}
		}
		return nil
	})
	if none {
		return all, nil
	}
	if admin {
		out := []events.Event{}
		for _, e := range all {
			if e.Target == "" || strings.EqualFold(e.Target, agent) {
				out = append(out, e)
			}
		}
		return out, nil
	}
	if !known {
		return nil, fmt.Errorf("pass agent: your Bot's name as connected to Rubi (agents: %s)", c.agentNames())
	}
	out := []events.Event{}
	for _, e := range all {
		if strings.EqualFold(e.Target, agent) || e.Target == "" && subs[e.Integration] {
			out = append(out, e)
		}
	}
	return out, nil
}

// Reasons AckFor gives (rubi_ack's `reason`); the first two are the store's.
const (
	AckAcked          = string(events.Acked)        // this agent's first ack
	AckAlreadyAcked   = string(events.AlreadyAcked) // this agent acked it before
	AckNotVisible     = "not_visible"               // the event exists but isn't addressed to this agent
	AckNotStored      = "not_stored"                // delivered to a webhook only; there is nothing to ack
	AckUnknownExpired = string(events.NotFound)     // no such event (never existed, pruned, or already gone)
)

// AckFor records that agent reported the event, if it may see it, and says why when it didn't: one of
// the Ack* reasons. Acks are per agent: another Bot still sees the event until it acks it too.
func (c *Core) AckFor(agent, code, id string) (string, error) {
	visible, err := c.EventsFor(agent, code, true)
	if err != nil {
		return "", err
	}
	// Ack under the registered name, so the key matches the one settleAcked checks.
	name := agent
	_ = c.Vault.View(func(d *vault.Data) error {
		if a := findAgent(d, agent); a != nil {
			name = a.Name
		}
		return nil
	})
	for _, e := range visible {
		if e.ID == id {
			res := c.Events.Ack(name, id)
			if res == events.Acked {
				c.settleAcked(id)
				c.saveEvents()
			}
			return string(res), nil
		}
	}
	if _, ok := c.Events.Get(id); ok {
		return AckNotVisible, nil
	}
	if events.NotStored(id) {
		return AckNotStored, nil
	}
	return AckUnknownExpired, nil
}

// settleAcked marks the event acked by everyone once no Bot is left that still sees it unacked, so it
// is pruned early like before acks were per agent.
func (c *Core) settleAcked(id string) {
	var names []string
	_ = c.Vault.View(func(d *vault.Data) error {
		for _, a := range d.Agents {
			names = append(names, a.Name)
		}
		return nil
	})
	for _, n := range names {
		list, _ := c.eventsVisible(n, false)
		for _, e := range list {
			if e.ID == id {
				return
			}
		}
	}
	c.Events.AckAll(id)
}
