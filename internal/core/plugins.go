package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/audit"
	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// action finds an action kind among installed plugins.
func (c *Core) action(kind string) (rubiplugin.Action, plugins.Manifest, bool) {
	for _, m := range c.Store.Installed() {
		if a, ok := m.Action(kind); ok {
			return a, m, true
		}
	}
	return rubiplugin.Action{}, plugins.Manifest{}, false
}

// accountOf finds a connected account of a plugin ("" = the default one).
func accountOf(d *vault.Data, plugin, ref string) (*vault.Account, error) {
	i := d.Integrations[plugin]
	if i == nil || len(i.Accounts) == 0 {
		return nil, errors.New("the plugin is not connected")
	}
	a := i.Find(ref)
	if a == nil {
		var names []string
		for _, x := range i.Accounts {
			names = append(names, x.Label)
		}
		return nil, fmt.Errorf("no connected account %q (connected: %s)", ref, strings.Join(names, ", "))
	}
	return a, nil
}

// enabled reports whether a plugin is connected (set up by the user).
func (c *Core) enabled(id string) bool {
	on := false
	_ = c.Vault.View(func(d *vault.Data) error {
		i := d.Integrations[id]
		on = i != nil && len(i.Accounts) > 0
		return nil
	})
	return on
}

func (c *Core) record(id string) *vault.Plugin {
	var r *vault.Plugin
	_ = c.Vault.View(func(d *vault.Data) error {
		if p := d.Plugins[id]; p != nil {
			cp := *p
			r = &cp
		}
		return nil
	})
	return r
}

// ---- lifecycle ----

// startPlugins starts every installed plugin whose files match the record in the vault.
func (c *Core) startPlugins() {
	var records map[string]vault.Plugin
	_ = c.Vault.View(func(d *vault.Data) error {
		records = map[string]vault.Plugin{}
		for id, p := range d.Plugins {
			records[id] = *p
		}
		return nil
	})
	for id, rec := range records {
		m, ok := c.Store.Get(id)
		if !ok || m.Version != rec.Version {
			c.pluginProblem(id, "plugin.missing", "The plugin's files are missing or don't match the installed version. Reinstall it from the store.")
			continue
		}
		go func() {
			if err := c.Runner.Start(id, rec.Version, rec.Tree); err != nil {
				log.Printf("[plugin %s] not started: %v", id, err)
				if errors.Is(err, plugins.ErrNotRunning) {
					return
				}
				c.pluginProblem(id, "plugin.failed", err.Error())
			}
		}()
	}
}

func (c *Core) stopPlugins() { c.Runner.StopAll() }

func (c *Core) pluginProblem(id, typ, msg string) {
	c.Audit.Record(typ, audit.Fields{"plugin": id, "detail": msg})
	if c.State() == Unlocked {
		c.Events.Emit("rubi", typ, map[string]any{"plugin": id, "detail": msg}, nil)
	}
}

// pluginLaunched tells a connected plugin to begin its work.
func (c *Core) pluginLaunched(id string) {
	if !c.enabled(id) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Runner.Call(ctx, id, "start", nil, nil); err != nil {
		log.Printf("[plugin %s] start: %v", id, err)
	}
}

func (c *Core) pluginCrashed(id string, err error) {
	msg := "The plugin keeps crashing, so Rubi stopped restarting it. Locking and unlocking Rubi tries again."
	if err != nil {
		msg += " Last error: " + err.Error()
	}
	c.pluginProblem(id, "plugin.crashed", msg)
}

// restartPluginWork makes a running plugin pick up new settings.
func (c *Core) restartPluginWork(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = c.Runner.Call(ctx, id, "stop", nil, nil)
	if err := c.Runner.Call(ctx, id, "start", nil, nil); err != nil {
		log.Printf("[plugin %s] start: %v", id, err)
	}
}

// ---- tools ----

// PluginTool is a tool offered to the agent by an installed plugin.
type PluginTool struct {
	Plugin string
	Tool   rubiplugin.Tool
}

// PluginTools lists the tools of all installed plugins (known even while locked).
func (c *Core) PluginTools() []PluginTool {
	var out []PluginTool
	for _, m := range c.Store.Installed() {
		for _, t := range m.Tools {
			out = append(out, PluginTool{Plugin: m.ID, Tool: t})
		}
	}
	return out
}

// OnToolsChanged registers a callback for plugin installs, updates and removals.
func (c *Core) OnToolsChanged(fn func()) {
	c.mkt.mu.Lock()
	c.mkt.toolsChanged = append(c.mkt.toolsChanged, fn)
	c.mkt.mu.Unlock()
}

func (c *Core) toolsChanged() {
	c.mkt.mu.Lock()
	fns := append([]func(){}, c.mkt.toolsChanged...)
	c.mkt.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// CallTool runs a plugin tool for the agent, or explains what the user has to do first.
func (c *Core) CallTool(ctx context.Context, name string, args json.RawMessage) (map[string]any, error) {
	var m plugins.Manifest
	found := false
	for _, pm := range c.Store.Installed() {
		if _, ok := pm.Tool(name); ok {
			m, found = pm, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("unknown tool %q; see rubi_store for installed plugins", name)
	}
	if st := c.State(); st != Unlocked {
		purpose := "unlock"
		if st == Unpaired {
			purpose = "pair"
		}
		out := map[string]any{"status": st, "message": "Rubi is " + string(st) + "; give the user the link below first."}
		if u, err := c.Link(purpose); err == nil {
			out["link"] = u
		} else {
			out["link_error"] = err.Error()
		}
		return out, nil
	}
	if !c.enabled(m.ID) {
		out := map[string]any{"status": "not_connected", "needs": m.Needs,
			"message": m.Name + " isn't connected yet. Give the user the setup link; they enter their details in the Rubi panel, never in the chat."}
		if u, err := c.Link("setup:" + m.ID); err == nil {
			out["link"] = u
		}
		return out, nil
	}
	var raw json.RawMessage
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	err := c.Runner.Call(ctx, m.ID, "tool", rubiplugin.ToolParams{Name: name, Arguments: args}, &raw)
	if errors.Is(err, plugins.ErrNotRunning) {
		return map[string]any{"status": "plugin_not_running",
			"message": m.Name + " isn't running (it may be starting, or it crashed). Try again shortly; if it persists, check rubi_events and tell the user."}, nil
	}
	if err != nil {
		return nil, err
	}
	return asObject(raw), nil
}

func asObject(raw json.RawMessage) map[string]any {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil && obj != nil {
		return obj
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	return map[string]any{"result": v}
}

// ---- requests from plugins ----

// pluginHandler answers a plugin's requests. Everything is confined to the plugin's own namespace.
func (c *Core) pluginHandler(id string) rubiplugin.Handler {
	return func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		m, ok := c.Store.Get(id)
		if !ok {
			return nil, errors.New("plugin is not installed")
		}
		if c.State() != Unlocked {
			return nil, errors.New("Rubi is locked")
		}
		switch method {
		case "settings.get":
			var in struct{ Account string }
			_ = json.Unmarshal(params, &in)
			var out json.RawMessage
			var acct string
			err := c.Vault.View(func(d *vault.Data) error {
				a, err := accountOf(d, id, in.Account)
				if err != nil {
					return err
				}
				out, acct = append(json.RawMessage(nil), a.Settings...), a.ID
				return nil
			})
			return map[string]any{"settings": out, "account": acct}, err
		case "accounts.list":
			var list []rubiplugin.AccountInfo
			_ = c.Vault.View(func(d *vault.Data) error {
				i := d.Integrations[id]
				if def := i.Find(""); def != nil {
					list = append(list, rubiplugin.AccountInfo{ID: def.ID, Label: def.Label, Default: true})
				}
				if i != nil {
					for _, a := range i.Accounts {
						if a != i.Find("") {
							list = append(list, rubiplugin.AccountInfo{ID: a.ID, Label: a.Label})
						}
					}
				}
				return nil
			})
			return map[string]any{"accounts": list}, nil
		case "secret.get":
			var in struct{ Key, Account string }
			_ = json.Unmarshal(params, &in)
			declared := false
			for _, s := range m.Secrets {
				declared = declared || s.Key == in.Key
			}
			if !declared {
				return nil, fmt.Errorf("secret %q is not declared in the manifest", in.Key)
			}
			var v string
			err := c.Vault.View(func(d *vault.Data) error {
				a, err := accountOf(d, id, in.Account)
				if err != nil {
					return err
				}
				if a.Secrets[in.Key] == "" {
					return fmt.Errorf("secret %q is not set", in.Key)
				}
				v = a.Secrets[in.Key]
				return nil
			})
			return map[string]any{"value": v}, err
		case "state.get":
			var out json.RawMessage
			_ = c.Vault.View(func(d *vault.Data) error {
				if i := d.Integrations[id]; i != nil {
					out = append(json.RawMessage(nil), i.State...)
				}
				return nil
			})
			if len(out) == 0 {
				out = json.RawMessage("null")
			}
			return map[string]any{"state": out}, nil
		case "state.set":
			var in struct {
				State json.RawMessage `json:"state"`
			}
			if err := json.Unmarshal(params, &in); err != nil {
				return nil, err
			}
			if len(in.State) > 1<<20 {
				return nil, errors.New("state is larger than 1 MiB")
			}
			return nil, c.Vault.Update(func(d *vault.Data) error {
				i := d.Integrations[id]
				if i == nil {
					return errors.New("the plugin is not connected")
				}
				i.State = in.State
				return nil
			})
		case "config.get":
			var in struct{ Account string }
			_ = json.Unmarshal(params, &in)
			return map[string]any{"config": c.pluginConfig(m, in.Account)}, nil
		case "level.get":
			var in struct{ Kind string }
			_ = json.Unmarshal(params, &in)
			if _, ok := m.Action(in.Kind); !ok {
				return nil, fmt.Errorf("action %q is not declared in the manifest", in.Kind)
			}
			return map[string]any{"level": c.PolicyLevel(in.Kind)}, nil
		case "approval.submit":
			return c.pluginSubmit(ctx, m, params)
		case "event.emit":
			var in struct {
				Type  string         `json:"type"`
				Data  map[string]any `json:"data"`
				Agent string         `json:"agent"`
			}
			if err := json.Unmarshal(params, &in); err != nil {
				return nil, err
			}
			et, ok := m.Event(in.Type)
			if !ok {
				return nil, fmt.Errorf("event type %q is not declared in the manifest", in.Type)
			}
			target := ""
			if in.Agent != "" && c.HasAgent(in.Agent) {
				target = in.Agent
			}
			ev := c.Events.EmitFor(target, false, id, in.Type, in.Data, et.Untrusted)
			return map[string]any{"event_id": ev.ID}, nil
		case "audit":
			var in struct {
				Event  string         `json:"event"`
				Fields map[string]any `json:"fields"`
			}
			_ = json.Unmarshal(params, &in)
			if in.Fields == nil {
				in.Fields = map[string]any{}
			}
			in.Fields["integration"] = id
			c.Audit.Record(id+"."+in.Event, in.Fields)
			return nil, nil
		}
		return nil, &rubiplugin.Error{Code: -32601, Message: "method not found: " + method}
	}
}

// pluginSubmit gates a plugin action. The approved action runs in the plugin with the submitted payload.
func (c *Core) pluginSubmit(ctx context.Context, m plugins.Manifest, params json.RawMessage) (any, error) {
	var in struct {
		Kind     string              `json:"kind"`
		Summary  string              `json:"summary"`
		Question string              `json:"question"`
		Preview  json.RawMessage     `json:"preview"`
		Options  []rubiplugin.Option `json:"options"`
		Items    []rubiplugin.Item   `json:"items"`
		Payload  json.RawMessage     `json:"payload"`
	}
	if err := json.Unmarshal(params, &in); err != nil {
		return nil, err
	}
	a, ok := m.Action(in.Kind)
	if !ok {
		return nil, fmt.Errorf("action %q is not declared in the manifest", in.Kind)
	}
	if !c.enabled(m.ID) {
		return nil, errors.New("the plugin is not connected")
	}
	opts := in.Options
	if len(opts) == 0 {
		opts = a.Options
	}
	if len(opts) == 0 {
		opts = []rubiplugin.Option{{Key: "approve", Label: "Approve"}}
	}
	var options []approvals.Option
	for _, o := range opts {
		options = append(options, approvals.Option{Key: o.Key, Label: o.Label, Meaning: o.Meaning})
	}
	var preview any
	_ = json.Unmarshal(in.Preview, &preview)
	if len(in.Items) > 100 {
		return nil, errors.New("a batch can have at most 100 items")
	}
	var items []approvals.Item
	seen := map[string]bool{}
	for _, it := range in.Items {
		if it.Key == "" || strings.ContainsAny(it.Key, ",") || seen[it.Key] {
			return nil, errors.New("batch items need unique keys without commas")
		}
		seen[it.Key] = true
		items = append(items, approvals.Item{Key: it.Key, Label: it.Label, Preview: it.Preview})
	}
	id, kind, payload := m.ID, in.Kind, in.Payload
	req := approvals.Request{Integration: id, Kind: kind, Summary: in.Summary, Question: in.Question,
		Preview: preview, Options: options, Items: items,
		Execute: func(ctx context.Context, option string) (any, error) {
			var raw json.RawMessage
			err := c.Runner.Call(ctx, id, "execute", rubiplugin.ExecuteParams{Kind: kind, Option: option, Payload: payload}, &raw)
			if err != nil {
				return nil, err
			}
			return asObject(raw), nil
		}}
	if c.PolicyLevel(kind) == approvals.None {
		opt := options[0].Key
		if len(items) > 0 {
			keys := make([]string, len(items))
			for i, it := range items {
				keys[i] = it.Key
			}
			opt = approvals.ItemsPrefix + strings.Join(keys, ",")
		}
		res, err := req.Execute(ctx, opt)
		c.Audit.Record("action.executed", audit.Fields{"kind": kind, "level": approvals.None, "ok": err == nil})
		if err != nil {
			return nil, err
		}
		return res, nil
	}
	return c.Approvals.Submit(ctx, req)
}
