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
//   - a plugin's events (e.g. replies) to the agent the plugin is assigned to;
//   - everything else to the default agent.

// AgentInfo is an agent as the agent and the panel see it (never the key).
type AgentInfo struct {
	Name    string `json:"name"`
	Host    string `json:"host"`
	Default bool   `json:"default"`
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
			out = append(out, AgentInfo{Name: a.Name, Host: redactURL(a.URL), Default: a == def})
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
func (c *Core) AddAgent(ctx context.Context, name, rawURL, key string) (string, error) {
	name, err := CleanAgentName(name)
	if err != nil {
		return "", err
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
	return c.RequestChange(ctx, "Let Rubi wake "+name, map[string]any{"agent": name, "webhook": redactURL(rawURL)},
		func(d *vault.Data) error {
			if a := findAgent(d, name); a != nil {
				a.URL, a.Key = rawURL, key
				return nil
			}
			d.Agents = append(d.Agents, &vault.Agent{Name: name, URL: rawURL, Key: key, Default: len(d.Agents) == 0})
			return nil
		}, nil)
}

// RemoveAgent requests approval to forget an agent. Its plugins fall back to the default agent.
func (c *Core) RemoveAgent(ctx context.Context, name string) (string, error) {
	if !c.HasAgent(name) {
		return "", fmt.Errorf("no agent %q (agents: %s)", name, c.agentNames())
	}
	return c.RequestChange(ctx, "Stop waking "+name, map[string]any{"agent": name,
		"effect": "Rubi won't send events to this Bot any more. Its plugins' events go to the default agent."},
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
			for _, i := range d.Integrations {
				if a != nil && strings.EqualFold(i.Agent, a.Name) {
					i.Agent = ""
				}
			}
			return nil
		}, nil)
}

// SetDefaultAgent requests approval to change which agent gets events that belong to no particular one.
func (c *Core) SetDefaultAgent(ctx context.Context, name string) (string, error) {
	if !c.HasAgent(name) {
		return "", fmt.Errorf("no agent %q (agents: %s)", name, c.agentNames())
	}
	return c.RequestChange(ctx, "Make "+name+" the default agent", map[string]any{"agent": name,
		"effect": "Gets Rubi's own events (updates, plugin problems) and those of plugins not assigned to another agent."},
		func(d *vault.Data) error {
			a := findAgent(d, name)
			for _, x := range d.Agents {
				x.Default = x == a
			}
			return nil
		}, nil)
}

// RoutePlugin requests approval to send a plugin's events (e.g. replies) to an agent ("" = default).
func (c *Core) RoutePlugin(ctx context.Context, plugin, agent string) (string, error) {
	m, ok := c.Store.Get(plugin)
	if !ok {
		return "", fmt.Errorf("plugin %q is not installed", plugin)
	}
	if agent != "" && !c.HasAgent(agent) {
		return "", fmt.Errorf("no agent %q (agents: %s)", agent, c.agentNames())
	}
	target := agent
	if target == "" {
		target = "the default agent"
	}
	return c.RequestChange(ctx, "Send "+m.Name+" events to "+target, map[string]any{"plugin": m.Name, "agent": target},
		func(d *vault.Data) error {
			i := d.Integrations[plugin]
			if i == nil {
				return errors.New(m.Name + " isn't connected yet")
			}
			i.Agent = agent
			return nil
		}, nil)
}

// pluginAgent is the agent a plugin's events go to ("" = default).
func (c *Core) pluginAgent(plugin string) string {
	name := ""
	_ = c.Vault.View(func(d *vault.Data) error {
		if i := d.Integrations[plugin]; i != nil {
			name = i.Agent
		}
		return nil
	})
	return name
}

// agentFor resolves an event target to an agent (unknown or "" falls back to the default).
func (c *Core) agentFor(target string) *vault.Agent {
	var out *vault.Agent
	_ = c.Vault.View(func(d *vault.Data) error {
		a := findAgent(d, target)
		if a == nil || target == "" {
			a = defaultAgent(d)
		}
		if a != nil {
			cp := *a
			out = &cp
		}
		return nil
	})
	return out
}
