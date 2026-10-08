package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/audit"
	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Rubi Home devices: helpers on a computer at the user's home (see internal/home). The user pairs one by
// pasting the code it prints into the panel. Plugins reach it only for what their manifest declares
// (manifest "home": "hue", "shortcuts").

var homeCapabilities = map[string]string{"hue": "Philips Hue", "shortcuts": "Shortcuts"}

type deviceClients struct {
	mu      sync.Mutex
	clients map[string]*homeproto.Client
}

func (c *Core) deviceClient(d *vault.Device) (*homeproto.Client, error) {
	c.devs.mu.Lock()
	defer c.devs.mu.Unlock()
	if c.devs.clients == nil {
		c.devs.clients = map[string]*homeproto.Client{}
	}
	if cl := c.devs.clients[d.ID]; cl != nil && cl.Token == d.Token {
		return cl, nil
	}
	cl, err := homeproto.NewClient(d.Relay, d.Bundle, d.Relays, d.Token)
	if err != nil {
		return nil, err
	}
	c.devs.clients[d.ID] = cl
	return cl, nil
}

func (c *Core) dropDeviceClient(id string) {
	c.devs.mu.Lock()
	defer c.devs.mu.Unlock()
	if cl := c.devs.clients[id]; cl != nil {
		cl.Close()
		delete(c.devs.clients, id)
	}
}

// Devices lists the paired helpers (without their tokens).
func (c *Core) Devices() []map[string]any {
	out := []map[string]any{}
	_ = c.Vault.View(func(d *vault.Data) error {
		for _, dev := range d.Devices {
			out = append(out, map[string]any{"id": dev.ID, "name": dev.Name, "paired": dev.Paired})
		}
		return nil
	})
	return out
}

func (c *Core) device(ref string) (*vault.Device, error) {
	var found *vault.Device
	var names []string
	_ = c.Vault.View(func(d *vault.Data) error {
		for _, dev := range d.Devices {
			names = append(names, dev.Name)
			if ref == "" && found == nil || dev.ID == ref || strings.EqualFold(dev.Name, ref) {
				cp := *dev
				found = &cp
			}
		}
		return nil
	})
	if found == nil {
		if len(names) == 0 {
			return nil, errors.New("no Rubi Home is paired: the user installs Rubi Home on a computer at home and pairs it in the Rubi panel (Settings > Rubi Home)")
		}
		return nil, fmt.Errorf("no Rubi Home called %q; paired: %s", ref, strings.Join(names, ", "))
	}
	return found, nil
}

// PairDevice pairs with a helper from the code it printed, then asks the user to approve adding it.
func (c *Core) PairDevice(ctx context.Context, code string) (string, error) {
	pc, err := homeproto.ParseCode(code)
	if err != nil {
		return "", err
	}
	token := homeproto.RandomToken()
	cl, err := homeproto.NewClient(pc.Relay, pc.Bundle, pc.Relays, token)
	if err != nil {
		return "", err
	}
	defer cl.Close()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var hello struct {
		Name   string `json:"name"`
		Folder string `json:"shortcuts_folder"`
	}
	if err := cl.Call(cctx, "pair", map[string]string{"secret": pc.Secret, "token": token, "label": "Rubi " + c.ID.Fingerprint()}, &hello); err != nil {
		return "", err
	}
	name := strings.TrimSpace(hello.Name)
	if name == "" {
		name = "Home"
	}
	dev := &vault.Device{ID: randomID(), Name: name, Relay: pc.Relay, Bundle: pc.Bundle, Relays: pc.Relays, Token: token,
		Paired: time.Now().UTC()}
	return c.RequestChange(ctx, "Add Rubi Home on "+name, map[string]any{"computer": name,
		"allows": "control Philips Hue on your home network and run the shortcuts in the folder \"" + hello.Folder + "\""},
		func(d *vault.Data) error {
			if len(d.Devices) >= 10 {
				return errors.New("at most 10 Rubi Home computers")
			}
			d.Devices = append(d.Devices, dev)
			return nil
		},
		func() { c.Audit.Record("device.paired", audit.Fields{"device": dev.ID, "name": name}) })
}

// RemoveDevice asks the user to approve forgetting a helper; the helper is told to forget us too.
func (c *Core) RemoveDevice(ctx context.Context, id string) (string, error) {
	dev, err := c.device(id)
	if err != nil || id == "" {
		return "", errors.New("unknown Rubi Home")
	}
	return c.RequestChange(ctx, "Remove Rubi Home on "+dev.Name, map[string]any{"computer": dev.Name},
		func(d *vault.Data) error {
			kept := d.Devices[:0]
			for _, x := range d.Devices {
				if x.ID != dev.ID {
					kept = append(kept, x)
				}
			}
			d.Devices = kept
			return nil
		},
		func() {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				if cl, err := c.deviceClient(dev); err == nil {
					_ = cl.Call(ctx, "unpair", nil, nil)
				}
				c.dropDeviceClient(dev.ID)
			}()
			c.Audit.Record("device.removed", audit.Fields{"device": dev.ID})
		})
}

// CheckDevice asks a helper whether it's there.
func (c *Core) CheckDevice(ctx context.Context, id string) (map[string]any, error) {
	dev, err := c.device(id)
	if err != nil {
		return nil, err
	}
	cl, err := c.deviceClient(dev)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var hello map[string]any
	if err := cl.Call(ctx, "hello", nil, &hello); err != nil {
		return map[string]any{"online": false, "problem": err.Error()}, nil
	}
	hello["online"] = true
	return hello, nil
}

// homeCall runs one helper operation for a plugin, within what its manifest declares.
func (c *Core) homeCall(ctx context.Context, plugin string, allowed []string, ref, op string, args json.RawMessage) (json.RawMessage, error) {
	ok := false
	for _, a := range allowed {
		ok = ok || strings.HasPrefix(op, a+".")
	}
	if !ok {
		return nil, fmt.Errorf("the manifest doesn't allow Rubi Home operation %q", op)
	}
	dev, err := c.device(ref)
	if err != nil {
		return nil, &rubiplugin.Error{Code: rubiplugin.CodeHomeNotPaired, Message: err.Error()}
	}
	cl, err := c.deviceClient(dev)
	if err != nil {
		return nil, &rubiplugin.Error{Code: rubiplugin.CodeHomeFailed, Message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Second)
	defer cancel()
	var out json.RawMessage
	if err := cl.Call(ctx, op, args, &out); err != nil {
		code := rubiplugin.CodeHomeFailed
		switch {
		case errors.Is(err, homeproto.ErrOffline):
			code = rubiplugin.CodeHomeOffline
		case err.Error() == homeproto.NotPairedMessage:
			code = rubiplugin.CodeHomeNotPaired
		}
		return nil, &rubiplugin.Error{Code: code, Message: dev.Name + ": " + err.Error()}
	}
	return out, nil
}
