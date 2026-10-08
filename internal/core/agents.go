package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

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
	key = strings.TrimSpace(key)
	if k, ok := strings.CutPrefix(strings.ToLower(key), "authorization:"); ok {
		key = strings.TrimSpace(key[len(key)-len(k):])
	}
	if strings.HasPrefix(strings.ToLower(key), "bearer ") {
		key = strings.TrimSpace(key[len("bearer "):])
	}
	if key == "" {
		return "", errors.New("the webhook key is missing")
	}
	preview := map[string]any{"agent": name, "webhook": redactURL(rawURL)}
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
			return nil
		}, nil)
}

// SetDefaultAgent requests approval to change which agent gets events that belong to no particular one.
func (c *Core) SetDefaultAgent(ctx context.Context, name string) (string, error) {
	if !c.HasAgent(name) {
		return "", fmt.Errorf("no agent %q (agents: %s)", name, c.agentNames())
	}
	return c.RequestChange(ctx, "Make "+name+" the default agent", map[string]any{"agent": name,
		"effect": "Gets the events no Bot subscribed to."},
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
			if a := findAgent(d, target); a != nil {
				out = append(out, *a)
				return nil
			}
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
		if len(out) == 0 {
			if a := defaultAgent(d); a != nil {
				out = append(out, *a)
			}
		}
		return nil
	})
	return out
}
