package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
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
	fields := append([]rubiplugin.ConfigField(nil), m.Config...)
	for i, f := range fields {
		if !f.Dynamic {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		var out struct {
			Options []rubiplugin.Option `json:"options"`
		}
		err := c.Runner.Call(ctx, id, "config.options", map[string]string{"key": f.Key}, &out)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("%s: couldn't load the choices: %w", f.Label, err)
		}
		fields[i].Options = out.Options
	}
	return map[string]any{"id": m.ID, "name": m.Name, "fields": fields, "values": c.pluginConfig(m)}, nil
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
		preview[f.Label] = describeConfig(f, v)
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

func describeConfig(f rubiplugin.ConfigField, v any) string {
	label := func(k string) string {
		for _, o := range f.Options {
			if o.Key == k {
				return o.Label
			}
		}
		return k
	}
	switch x := v.(type) {
	case string:
		if f.Type == "choice" {
			return label(x)
		}
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
			parts[i] = label(fmt.Sprint(e))
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprint(v)
}
