package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/audit"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// Hooks are private web addresses a plugin hands out, for things that can only send a plain web request:
// an iPhone Shortcut reporting "arrived home", say. An address is <base>/h/<route>/<id>. The route picks
// this Rubi on Rubi Gateway (it subscribes to it there, never on public relays) and the id one hook. Both
// are random; anyone holding the address can trigger the hook, so plugins treat what arrives as untrusted
// and the user can replace an address at any time. Hook requests reach the plugin as a "hook" call.

const hooksPerMinute = 20

var errNoHooks = errors.New("hook addresses need Rubi Gateway or Tailscale as Rubi's transport (see `rubi transport`)")

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// HookRoute is this Rubi's hook route, "" while there are no hooks or Rubi is locked.
func (c *Core) HookRoute() string {
	if c.State() != Unlocked {
		return ""
	}
	var route string
	_ = c.Vault.View(func(d *vault.Data) error {
		if d.Hooks != nil && len(d.Hooks.List) > 0 {
			route = d.Hooks.Route
		}
		return nil
	})
	return route
}

// HookURL returns the address of a plugin's hook, creating it (or a new one, with rotate) as needed.
func (c *Core) HookURL(plugin, account, name string, rotate bool) (string, error) {
	base := ""
	if c.HookBase != nil {
		base = strings.TrimRight(c.HookBase(), "/")
	}
	if base == "" {
		return "", errNoHooks
	}
	if name == "" || len(name) > 64 {
		return "", errors.New("give the hook a name (up to 64 characters)")
	}
	var route, id string
	created := false
	err := c.Vault.Update(func(d *vault.Data) error {
		if d.Hooks == nil {
			d.Hooks = &vault.Hooks{}
		}
		if d.Hooks.Route == "" {
			d.Hooks.Route = randomID()
		}
		route = d.Hooks.Route
		kept := d.Hooks.List[:0]
		for _, h := range d.Hooks.List {
			if h.Plugin == plugin && h.Account == account && h.Name == name {
				if !rotate {
					id = h.ID
				} else {
					continue
				}
			}
			kept = append(kept, h)
		}
		d.Hooks.List = kept
		if id == "" {
			if len(d.Hooks.List) >= 200 {
				return errors.New("too many hook addresses")
			}
			id, created = randomID(), true
			d.Hooks.List = append(d.Hooks.List, &vault.Hook{ID: id, Plugin: plugin, Account: account, Name: name,
				Created: time.Now().UTC()})
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if created {
		c.Audit.Record("hook.created", audit.Fields{"plugin": plugin, "name": name, "rotated": rotate})
		if c.OnHookRoute != nil {
			c.OnHookRoute()
		}
	}
	return base + "/h/" + route + "/" + id, nil
}

// DeliverHookAt handles a request that arrived directly at this Rubi (Tailscale transport).
func (c *Core) DeliverHookAt(route, id string, body []byte) bool {
	mine := c.HookRoute()
	if mine == "" || subtle.ConstantTimeCompare([]byte(route), []byte(mine)) != 1 {
		return false
	}
	return c.DeliverHook(id, body)
}

// DeliverHook passes a hook request to its plugin. It reports whether the hook exists.
func (c *Core) DeliverHook(id string, body []byte) bool {
	if c.State() != Unlocked {
		return false
	}
	var hook *vault.Hook
	_ = c.Vault.View(func(d *vault.Data) error {
		if d.Hooks == nil {
			return nil
		}
		for _, h := range d.Hooks.List {
			if subtle.ConstantTimeCompare([]byte(h.ID), []byte(id)) == 1 {
				cp := *h
				hook = &cp
			}
		}
		return nil
	})
	if hook == nil || !c.hookAllowed(hook.ID) {
		return false
	}
	if m, ok := c.Store.Get(hook.Plugin); !ok || !m.Hooks {
		return false
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		in := map[string]any{"name": hook.Name, "account": hook.Account, "body": string(body),
			"received_at": time.Now().UTC()}
		if err := c.Runner.Call(ctx, hook.Plugin, "hook", in, nil); err != nil {
			log.Printf("[plugin %s] hook %s: %v", hook.Plugin, hook.Name, err)
		}
	}()
	return true
}

func (c *Core) hookAllowed(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hookHit == nil {
		c.hookHit = map[string][]time.Time{}
	}
	now := time.Now()
	recent := c.hookHit[id][:0]
	for _, t := range c.hookHit[id] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= hooksPerMinute {
		c.hookHit[id] = recent
		return false
	}
	c.hookHit[id] = append(recent, now)
	return true
}

// dropHooks forgets the hooks of a plugin ("" account = all of them), e.g. when it is disconnected, so
// a later account with the same id doesn't inherit an address that was handed out before.
func dropHooks(d *vault.Data, plugin, account string) {
	if d.Hooks == nil {
		return
	}
	kept := d.Hooks.List[:0]
	for _, h := range d.Hooks.List {
		if h.Plugin == plugin && (account == "" || h.Account == account || h.Account == "" && len(d.Integrations[plugin].Accounts) <= 1) {
			continue
		}
		kept = append(kept, h)
	}
	d.Hooks.List = kept
}
