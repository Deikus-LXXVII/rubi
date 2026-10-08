package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// pluginConfig returns a plugin's user-only settings as they apply to one account ("" = default):
// stored values over manifest defaults; PerAccount fields come from that account.
func (c *Core) pluginConfig(m plugins.Manifest, account string) map[string]any {
	out := map[string]any{}
	for _, f := range m.Config {
		out[f.Key] = f.Default
	}
	_ = c.Vault.View(func(d *vault.Data) error {
		i := d.Integrations[m.ID]
		if i == nil {
			return nil
		}
		a := i.Find(account)
		for _, f := range m.Config {
			src := i.Config
			if f.PerAccount {
				if a == nil {
					continue
				}
				src = a.Config
			}
			if raw, ok := src[f.Key]; ok {
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

// PluginConfig is what the panel shows: the fields (with dynamic choices loaded for the account), the
// current values, and the connected accounts.
func (c *Core) PluginConfig(id, account string) (map[string]any, error) {
	m, ok := c.Store.Get(id)
	if !ok {
		return nil, fmt.Errorf("plugin %q is not installed", id)
	}
	var accounts []map[string]any
	_ = c.Vault.View(func(d *vault.Data) error {
		i := d.Integrations[id]
		if a := i.Find(account); a != nil {
			account = a.ID
		}
		if i != nil {
			for _, a := range i.Accounts {
				accounts = append(accounts, map[string]any{"id": a.ID, "label": a.Label, "default": a == i.Find("")})
			}
		}
		return nil
	})
	fields := append([]rubiplugin.ConfigField(nil), m.Config...)
	for i, f := range fields {
		if !f.Dynamic {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		var out struct {
			Options []rubiplugin.Option `json:"options"`
		}
		err := c.Runner.Call(ctx, id, "config.options", map[string]string{"key": f.Key, "account": account}, &out)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("%s: couldn't load the choices: %w", f.Label, err)
		}
		fields[i].Options = out.Options
	}
	return map[string]any{"id": m.ID, "name": m.Name, "fields": fields, "values": c.pluginConfig(m, account),
		"account": account, "accounts": accounts}, nil
}

// SetPluginConfig requests approval to change a plugin's user-only settings (PerAccount ones for the
// given account). Only the panel calls this; the agent has no way to change them.
func (c *Core) SetPluginConfig(ctx context.Context, id, account string, values map[string]any) (string, error) {
	m, ok := c.Store.Get(id)
	if !ok {
		return "", fmt.Errorf("plugin %q is not installed", id)
	}
	acctID, acctLabel := "", ""
	_ = c.Vault.View(func(d *vault.Data) error {
		if a := d.Integrations[id].Find(account); a != nil {
			acctID, acctLabel = a.ID, a.Label
		}
		return nil
	})
	current := c.pluginConfig(m, acctID)
	type change struct {
		perAccount bool
		raw        json.RawMessage
	}
	changes := map[string]change{}
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
		changes[f.Key] = change{perAccount: f.PerAccount, raw: raw}
		label := f.Label
		if f.PerAccount && acctLabel != "" {
			label += " (" + acctLabel + ")"
			preview["account"] = acctLabel
		}
		preview[label] = describeConfig(f, v)
	}
	if len(changes) == 0 {
		return "", fmt.Errorf("nothing changed")
	}
	return c.RequestChange(ctx, "Change "+m.Name+" settings", preview, func(d *vault.Data) error {
		i := d.Integrations[id]
		if i == nil || len(i.Accounts) == 0 {
			return fmt.Errorf("%s isn't connected yet", m.Name)
		}
		for k, ch := range changes {
			if ch.perAccount {
				a := i.Find(acctID)
				if a == nil {
					return errors.New("that account isn't connected any more")
				}
				if a.Config == nil {
					a.Config = map[string]json.RawMessage{}
				}
				a.Config[k] = ch.raw
				continue
			}
			if i.Config == nil {
				i.Config = map[string]json.RawMessage{}
			}
			i.Config[k] = ch.raw
		}
		return nil
	}, func() {
		go func() { // let the plugin react now (e.g. start watching), not at its next check
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = c.Runner.Call(ctx, id, "config.changed", map[string]string{"account": acctID}, nil)
		}()
	})
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
