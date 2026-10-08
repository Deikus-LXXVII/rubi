package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// pluginConfig returns a plugin's user-only settings: stored values over manifest defaults.
func (c *Core) pluginConfig(m plugins.Manifest) map[string]any {
	out := map[string]any{}
	for _, f := range m.Config {
		out[f.Key] = f.Default
	}
	_ = c.Vault.View(func(d *vault.Data) error {
		i := d.Integrations[m.ID]
		if i == nil {
			return nil
		}
		for _, f := range m.Config {
			if raw, ok := i.Config[f.Key]; ok {
				var v any
				if json.Unmarshal(raw, &v) == nil {
					out[f.Key] = v
				}
			}
		}
		return nil
	})
	return out
}

// PluginConfig is what the panel shows: the fields and their current values.
func (c *Core) PluginConfig(id string) (map[string]any, error) {
	m, ok := c.Store.Get(id)
	if !ok {
		return nil, fmt.Errorf("plugin %q is not installed", id)
	}
	return map[string]any{"id": m.ID, "name": m.Name, "fields": m.Config, "values": c.pluginConfig(m)}, nil
}

// SetPluginConfig requests approval to change a plugin's user-only settings. Only the panel calls this;
// the agent has no way to change them.
func (c *Core) SetPluginConfig(ctx context.Context, id string, values map[string]any) (string, error) {
	m, ok := c.Store.Get(id)
	if !ok {
		return "", fmt.Errorf("plugin %q is not installed", id)
	}
	current := c.pluginConfig(m)
	changes := map[string]json.RawMessage{}
	preview := map[string]any{"plugin": m.Name}
	for _, f := range m.Config {
		v, ok := values[f.Key]
		if !ok {
			continue
		}
		if err := plugins.CheckConfigValue(f, v); err != nil {
			return "", fmt.Errorf("%s: %w", f.Label, err)
		}
		raw, _ := json.Marshal(v)
		old, _ := json.Marshal(current[f.Key])
		if string(raw) == string(old) {
			continue
		}
		changes[f.Key] = raw
		preview[f.Label] = describeConfig(v)
	}
	if len(changes) == 0 {
		return "", fmt.Errorf("nothing changed")
	}
	return c.RequestChange(ctx, "Change "+m.Name+" settings", preview, func(d *vault.Data) error {
		i := d.Integrations[id]
		if i == nil {
			return fmt.Errorf("%s isn't connected yet", m.Name)
		}
		if i.Config == nil {
			i.Config = map[string]json.RawMessage{}
		}
		for k, v := range changes {
			i.Config[k] = v
		}
		return nil
	}, nil)
}

func describeConfig(v any) string {
	switch x := v.(type) {
	case bool:
		if x {
			return "On"
		}
		return "Off"
	case []any:
		if len(x) == 0 {
			return "(empty)"
		}
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = fmt.Sprint(e)
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprint(v)
}
