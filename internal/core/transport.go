package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/audit"
)

// How the user's browser reaches the panel (the daemon stores and applies the choice; see daemon/transport.go).
// The panel shows the current one in Settings and lets the user change it behind a passkey approval.

// The transport names, as the daemon stores them.
const (
	TransportGateway   = "gateway"
	TransportRelays    = "relays"
	TransportTailscale = "tailscale"
)

// TransportHooks are what the daemon gives core: how to store a choice, and whether Tailscale is there.
type TransportHooks struct {
	Save func(transport string) error
	// Current reads the stored choice (what the next start will use), which can differ from the one in use while a
	// restart is pending.
	Current func() string
	// TailscaleReady says whether Tailscale is running and signed in on this machine right now.
	TailscaleReady func() bool
}

// SetTransportHooks is called by the daemon.
func (c *Core) SetTransportHooks(h TransportHooks) {
	c.upd.mu.Lock()
	c.tr = h
	c.upd.mu.Unlock()
}

func (c *Core) transportHooks() TransportHooks {
	c.upd.mu.Lock()
	defer c.upd.mu.Unlock()
	return c.tr
}

type transportOption struct {
	key, label, help string
}

var transportOptions = []transportOption{
	{TransportGateway, "Rubi Gateway", "A relay run for Rubi only, with public relays as a backup. Works from any network, and gives plugins private hook addresses."},
	{TransportRelays, "Public relays only", "No Rubi-run server in the path. Works from any network, but plugins can't have hook addresses."},
	{TransportTailscale, "Tailscale", "Reachable only from your own devices on your Tailscale network, which must be signed in on the phone or computer you use."},
}

// TransportSettings is what the panel's Connection section shows: the transport in use and the choices.
func (c *Core) TransportSettings() map[string]any {
	cur := c.storedTransport()
	ready := false
	if f := c.transportHooks().TailscaleReady; f != nil {
		ready = f()
	}
	opts := []any{}
	for _, o := range transportOptions {
		m := map[string]any{"key": o.key, "label": o.label, "help": o.help, "available": true}
		if o.key == TransportTailscale && !ready && cur != TransportTailscale {
			m["available"] = false
			m["help"] = o.help + " Tailscale isn't installed on the machine where Rubi runs: ask your agent to set it up, then come back here."
		}
		opts = append(opts, m)
	}
	out := map[string]any{"current": cur, "options": opts, "info": c.TransportInfo()}
	if fb := c.transportFallback(); fb != "" {
		out["fallback"] = fb
		out["notice"] = "Tailscale isn't working, so Rubi is reachable through Rubi Gateway or the public relays until Tailscale works or you pick a connection here."
	}
	return out
}

// transportEffect says what the user must know before approving a switch from one transport to another.
func transportEffect(from, to string) string {
	switch {
	case to == TransportTailscale:
		return "Rubi restarts and stays unlocked. The link open on this page stops working, because it points at the old address. " +
			"Ask your agent for a new link and open it on a device signed in to your Tailscale network."
	case from == TransportTailscale:
		return "Rubi restarts and stays unlocked. The link open on this page stops working, because it points at your Tailscale address. " +
			"Ask your agent for a new link and open it."
	}
	return "Rubi restarts in a few seconds and stays unlocked. This page keeps working: both choices use the same public relays."
}

// MarkStartedByTransportSwitch is called by the daemon when this process was started by the restart of a panel
// connection change. Only then may a failing Tailscale fall back to Rubi Gateway (see the daemon).
func (c *Core) MarkStartedByTransportSwitch() {
	c.mu.Lock()
	c.startedBySwitch = true
	c.mu.Unlock()
}

// StartedByTransportSwitch says whether this process was started by a panel connection change.
func (c *Core) StartedByTransportSwitch() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.startedBySwitch
}

// storedTransport is the choice the next start will use.
func (c *Core) storedTransport() string {
	if f := c.transportHooks().Current; f != nil {
		if v := f(); v != "" {
			return v
		}
	}
	cur, _ := c.TransportInfo()["transport"].(string)
	return cur
}

// restartingSettings is a connection-change approval that has armed the restart (its result says so).
func restartingSettings(s approvals.Snapshot) bool {
	m, _ := s.Result.(map[string]any)
	return s.Kind == "rubi.settings" && s.State == approvals.Executed && m["restarting"] == true && m["transport"] != nil
}

// ChooseTransport asks the user to approve changing how the panel reaches Rubi. After the approval it stores the
// choice and restarts Rubi the way an update does (it stays unlocked). Nothing changes before the approval, and
// everything it relies on is checked again when the approval is executed.
func (c *Core) ChooseTransport(ctx context.Context, to string) (string, error) {
	if c.State() != Unlocked {
		return "", errors.New("Rubi is locked; unlock it first")
	}
	label := ""
	for _, o := range transportOptions {
		if o.key == to {
			label = o.label
		}
	}
	if label == "" {
		return "", errors.New("the connection must be \"gateway\", \"relays\" or \"tailscale\"")
	}
	hooks := c.transportHooks()
	if hooks.Save == nil {
		return "", errors.New("this Rubi can't change its connection")
	}
	if c.storedTransport() == to {
		return "", errors.New("Rubi already uses " + label)
	}
	errTailscale := errors.New("Tailscale isn't running and signed in on the machine where Rubi runs; ask your agent to set it up first")
	if to == TransportTailscale && (hooks.TailscaleReady == nil || !hooks.TailscaleReady()) {
		return "", errTailscale
	}
	from, _ := c.TransportInfo()["transport"].(string) // the one in use: what this page reaches Rubi through
	if fb := c.transportFallback(); fb != "" {
		from = fb
	}
	preview := map[string]any{"now": from, "after": to, "what": "Changes how this panel reaches Rubi: " + label, "effect": transportEffect(from, to)}
	if to == TransportTailscale {
		preview["needs"] = "Tailscale must be running and signed in on the machine where Rubi runs, and on the device you open the new link on"
	}
	if to == TransportRelays {
		preview["hooks"] = "Hook addresses (for example an iPhone Shortcut reporting that you arrived home) stop working until you choose Rubi Gateway or Tailscale again"
	}
	return c.requestSettings(ctx, "Reach Rubi through "+label, preview, func(context.Context) (any, error) {
		// Things may have changed since the user was asked: another approval may have run, Tailscale may have gone.
		stored := c.storedTransport()
		if stored == to {
			return nil, errors.New("Rubi already uses " + label + " (it was changed since you were asked); nothing was done")
		}
		if to == TransportTailscale && (hooks.TailscaleReady == nil || !hooks.TailscaleReady()) {
			c.Audit.Record("transport.failed", audit.Fields{"to": to, "reason": "tailscale"})
			return nil, errTailscale
		}
		exe, err := executable()
		if err != nil {
			c.Audit.Record("transport.failed", audit.Fields{"to": to, "reason": "exe"})
			return nil, errors.New("Rubi can't find its own program to restart it, so nothing was changed; ask your agent to restart Rubi")
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		return c.applyTransport(hooks, stored, to, label, from, exe)
	})
}

// executable is a seam: where the running program is.
var executable = os.Executable

// applyTransport stores the choice, arms the restart and only then records the change.
func (c *Core) applyTransport(hooks TransportHooks, stored, to, label, inUse, exe string) (any, error) {
	if err := hooks.Save(to); err != nil {
		c.Audit.Record("transport.failed", audit.Fields{"to": to, "reason": "save"})
		return nil, err
	}
	shutdown := c.armRestart(exe, restartTransport)
	c.Audit.Record("transport.changed", audit.Fields{"from": stored, "to": to})
	if shutdown == nil {
		return map[string]any{"restarting": false, "transport": to,
			"receipt": "Saved: Rubi will reach this panel through " + label + " the next time it starts. Ask your agent to restart Rubi."}, nil
	}
	// Give the panel a moment to receive the result before the process restarts.
	time.AfterFunc(1500*time.Millisecond, shutdown)
	return map[string]any{"restarting": true, "transport": to,
		"receipt": "Rubi now reaches this panel through " + label + " and is restarting. " + transportEffect(inUse, to)}, nil
}
