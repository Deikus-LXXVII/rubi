package core

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Deikus-LXXVII/rubi/internal/audit"
	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Rubi Home devices: helpers on a computer at the user's home (see internal/home). The user pairs one by
// pasting the code it prints into the panel. Plugins reach it only for what their manifest declares
// (manifest "home": "hue", "shortcuts", "claude", "claude-sessions", "claude-start", "claude-agent", "agent-setup",
// "t3-agent", "t3-setup").

var homeCapabilities = map[string]string{"hue": "Philips Hue", "shortcuts": "Shortcuts",
	"claude":          "answering Claude Code's permission requests on that computer (it can allow or deny commands)",
	"claude-sessions": "listing the Claude Code sessions on that computer and passing messages to and from them (it can't approve commands)",
	"claude-start":    "starting a new Claude Code session on that computer with a first message; in sessions that skip permission prompts, Claude can run any command there without asking",
	"claude-agent":    "running Claude agents in a virtual machine on that computer: starting and steering their sessions, approving or denying each tool call, and allowing git pushes with your credentials",
	"agent-setup":     "setting up Claude agents on that computer: installing Apple's container tool (macOS asks for the administrator password itself), creating their network, building their image, adding project folders and signing in to Claude in their virtual machine",
	"t3-agent":        "running Claude agents through T3 in a virtual machine on that computer: starting and steering their sessions, approving or denying each tool call, and allowing git pushes with your credentials",
	"t3-setup":        "setting up Claude agents through T3 on that computer: downloading and starting the pinned T3 server and Node (checked against fixed hashes), pairing with it, installing Apple's container tool (macOS asks for the administrator password itself), creating its network, building the image, adding project folders and signing in to Claude in the virtual machine"}

type deviceClients struct {
	mu       sync.Mutex
	clients  map[string]*homeproto.Client
	hookSeen map[string]float64 // when the last installation result of each computer was audited
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

// PairDevice pairs with a helper from its code (a code from a link has no secret: the PIN in Rubi Home's window is it), then asks the user to approve adding it.
func (c *Core) PairDevice(ctx context.Context, code, pin string) (string, error) {
	pc, err := homeproto.ParseCodeWithPin(code, pin)
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
		Name     string `json:"name"`
		Folder   string `json:"shortcuts_folder"`
		Features map[string]struct {
			Available bool `json:"available"`
		} `json:"features"`
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
	sum := sha256.Sum256([]byte(pc.Bundle))
	fingerprint := base64.RawURLEncoding.EncodeToString(sum[:6])
	f, known := hello.Features["shortcuts"]
	shortcuts := !known || f.Available // a Rubi Home that sends no list is an older one: it is a Mac
	return c.RequestChange(ctx, "Add Rubi Home on "+name, map[string]any{"computer (as it calls itself)": name,
		"fingerprint": fingerprint + " (Rubi Home shows the same fingerprint when you open it)",
		"check":       "Approve only if you just opened Rubi Home on your own computer and used its code",
		"allows":      allowsText(hello.Folder, shortcuts)},
		func(d *vault.Data) error {
			if len(d.Devices) >= 10 {
				return errors.New("at most 10 Rubi Home computers")
			}
			d.Devices = append(d.Devices, dev)
			return nil
		},
		func() {
			c.Audit.Record("device.paired", audit.Fields{"device": dev.ID, "name": name})
			// A new pairing replaces the old one on the computer, so it learns the library key again.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_ = c.syncLibraryHome(ctx, dev, true)
			}()
		})
}

// allowsText is what the approval says a paired Rubi Home allows. Shortcuts are named only when that computer has them.
func allowsText(folder string, shortcuts bool) string {
	run := ""
	if shortcuts {
		run = "run the shortcuts in the folder \"" + folder + "\", "
	}
	return "control Philips Hue on your home network, " + run +
		"answer Claude Code's permission requests there and pass messages to and from its Claude Code sessions (only plugins you approve for that can)"
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
	c.auditHookResult(dev.ID, hello)
	return hello, nil
}

// ListDeviceFolders lists the folders in path (empty: the home folder) on a Home computer, for the panel's folder
// browser. Only the panel asks for it (panelapi: a full settings link); a plugin's call for "fs.list" is refused by
// attestHomeCall, and no manifest can declare the capability. Home answers folder names and nothing else, and
// refuses the places a project can never be in. The panel draws the answer as text.
func (c *Core) ListDeviceFolders(ctx context.Context, id, path string) (map[string]any, error) {
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
	var out map[string]any
	if err := cl.Call(ctx, opFsList, map[string]any{"path": path}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// hookCallTimeout bounds each call to Home about the hook. Both answer at once: the install only starts
// (the administrator dialog stays open on Home for minutes; the panel watches `hello.hook` for the end).
const hookCallTimeout = 30 * time.Second

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// clipRunes shortens text from Home to n runes (never in the middle of one).
func clipRunes(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// InstallHookOnDevice asks the user to approve installing (or updating) the root-owned Claude Code hook on a
// Home computer. Home is asked first, without a prompt, what it would install (hook.plan: where, which version,
// the SHA-256 of the file), and that goes into the approval. Nothing is installed before the approval; its
// execute step starts the installation on Home, where macOS shows the administrator dialog.
func (c *Core) InstallHookOnDevice(ctx context.Context, id string) (string, error) {
	dev, err := c.device(id)
	if err != nil || id == "" {
		return "", errors.New("unknown Rubi Home")
	}
	cl, err := c.deviceClient(dev)
	if err != nil {
		return "", err
	}
	pctx, cancel := context.WithTimeout(ctx, hookCallTimeout)
	defer cancel()
	var plan struct {
		Target     string `json:"target"`
		Version    string `json:"version"`
		SHA256     string `json:"sha256"`
		PlanSHA256 string `json:"plan_sha256"` // everything that would run as administrator
		Admin      string `json:"admin"`       // what the person at that computer will be asked
		Plugin     bool   `json:"plugin"`
	}
	if err := cl.Call(pctx, "hook.plan", map[string]any{}, &plan); err != nil {
		c.Audit.Record("device.hook.failed", audit.Fields{"device": dev.ID, "reason": "refused"})
		return "", err
	}
	preview := map[string]any{"computer": dev.Name,
		"what": "Rubi Home copies itself to a protected place on that computer, so Claude Code can ask Rubi before it acts"}
	if plan.Target != "" {
		preview["target"] = clipRunes(plan.Target, 200)
	}
	if plan.Version != "" {
		preview["version"] = clipRunes(plan.Version, 40)
	}
	if sha256Hex.MatchString(plan.SHA256) {
		preview["sha256"] = plan.SHA256
	}
	if sha256Hex.MatchString(plan.PlanSHA256) {
		// What the approval is for: if anything that would be installed changes before it runs, this hash changes and nothing
		// is installed.
		preview["plan"] = plan.PlanSHA256
	}
	if plan.Plugin {
		preview["plugin"] = "yes: the Claude Code sister tools (a plugin) are installed with it"
	} else {
		preview["plugin"] = "no"
	}
	preview["check"] = "macOS will ask for the administrator password on that computer; approve only if you asked for this"
	if a := strings.TrimSpace(plan.Admin); a != "" {
		preview["check"] = clipRunes(a, 300)
	}
	return c.requestSettings(ctx, "Install the Claude Code hook on "+dev.Name, preview,
		func(ctx context.Context) (any, error) {
			cctx, cancel := context.WithTimeout(ctx, hookCallTimeout)
			defer cancel()
			var res map[string]any
			args := map[string]any{}
			if sha256Hex.MatchString(plan.PlanSHA256) {
				args["plan_sha256"] = plan.PlanSHA256 // Home refuses unless the plan is still this one
			}
			if plan.Plugin {
				args["with_plugin"] = true
			}
			if err := cl.Call(cctx, "hook.install", args, &res); err != nil {
				c.Audit.Record("device.hook.failed", audit.Fields{"device": dev.ID, "reason": hookCallReason(err)})
				return nil, err
			}
			if sudo, _ := res["needs_bot_sudo"].(bool); sudo {
				return c.botSudoResult(dev, plan.PlanSHA256, res)
			}
			if started, _ := res["started"].(bool); !started {
				c.Audit.Record("device.hook.failed", audit.Fields{"device": dev.ID, "reason": "refused"})
				return nil, errors.New("Rubi Home didn't start the installation")
			}
			c.Audit.Record("device.hook.started", audit.Fields{"device": dev.ID})
			receipt := "macOS is asking for the administrator password on " + dev.Name + "."
			if a := plan.Admin; a != "" && !strings.Contains(a, "macOS") {
				receipt = "The administrator prompt is open on " + dev.Name + ". Answer it there."
			}
			return map[string]any{"started": true, "receipt": receipt}, nil
		})
}

var hueBridgeID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ForgetHueBridge asks the user to approve forgetting the certificate Rubi Home recorded for one Hue Bridge. That
// record is what keeps a look-alike device on the home network from taking the bridge's place, so forgetting it
// lets the next pairing trust whatever the bridge then shows. Only the approval's execute step asks Home; a plugin
// can't (the home gate refuses hue.forget).
func (c *Core) ForgetHueBridge(ctx context.Context, id, bridge string) (string, error) {
	dev, err := c.device(id)
	if err != nil || id == "" {
		return "", errors.New("unknown Rubi Home")
	}
	bridge = strings.ToLower(strings.TrimSpace(bridge))
	if !hueBridgeID.MatchString(bridge) {
		return "", errors.New("not a Hue Bridge id")
	}
	cl, err := c.deviceClient(dev)
	if err != nil {
		return "", err
	}
	return c.requestSettings(ctx, "Forget the Hue Bridge "+bridge+" on "+dev.Name, map[string]any{
		"computer": dev.Name, "bridge": bridge,
		"what":  "Rubi Home forgets the certificate it recorded for this bridge, and trusts what the bridge shows the next time you set Hue up",
		"check": "Approve only if you replaced or reset this bridge; otherwise something else on your network may be pretending to be it"},
		func(ctx context.Context) (any, error) {
			cctx, cancel := context.WithTimeout(ctx, hookCallTimeout)
			defer cancel()
			var res map[string]any
			if err := cl.Call(cctx, "hue.forget", map[string]string{"id": bridge}, &res); err != nil {
				c.Audit.Record("device.hue.forget.failed", audit.Fields{"device": dev.ID, "bridge": bridge, "reason": hookCallReason(err)})
				return nil, err
			}
			c.Audit.Record("device.hue.forgotten", audit.Fields{"device": dev.ID, "bridge": bridge})
			return map[string]any{"forgotten": res["forgotten"] == true,
				"receipt": "Rubi Home on " + dev.Name + " forgot that bridge. Set Hue up again."}, nil
		})
}

// auditHookResult records how an installation ended, once, when a check of the computer first shows it.
func (c *Core) auditHookResult(devID string, hello map[string]any) {
	hook, _ := hello["hook"].(map[string]any)
	last, _ := hook["last"].(map[string]any)
	at, _ := last["at"].(float64)
	result, _ := last["result"].(string)
	event, reason := hookResultAudit(result)
	if event == "" {
		return
	}
	c.devs.mu.Lock()
	if c.devs.hookSeen == nil || len(c.devs.hookSeen) > 1000 {
		c.devs.hookSeen = map[string]float64{}
	}
	seen := c.devs.hookSeen[devID] == at
	c.devs.hookSeen[devID] = at
	c.devs.mu.Unlock()
	if seen {
		return
	}
	if reason == "" {
		c.Audit.Record(event, audit.Fields{"device": devID})
		return
	}
	c.Audit.Record(event, audit.Fields{"device": devID, "reason": reason})
}

// hookCallReason is the audit reason for a call to Home that failed: "failed" when Home couldn't be reached,
// "refused" when it answered and said no.
func hookCallReason(err error) string {
	if errors.Is(err, homeproto.ErrOffline) {
		return "failed"
	}
	return "refused"
}

// hookResultAudit maps a result Home reports to the audit event and its short reason code ("" for none).
// Anything unknown is not recorded.
func hookResultAudit(result string) (event, reason string) {
	switch result {
	case "ok":
		return "device.hook.installed", ""
	case "cancelled", "timeout", "failed":
		return "device.hook.failed", result
	}
	return "", ""
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
	args, err := c.attestHomeCall(plugin, op, args)
	if err != nil {
		return nil, &rubiplugin.Error{Code: rubiplugin.CodeHomeFailed, Message: err.Error()}
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
	if homeOp(op) == opProjectAdd {
		if err := c.checkProjectResolves(ctx, cl, op, args); err != nil {
			return nil, &rubiplugin.Error{Code: rubiplugin.CodeHomeFailed, Message: err.Error()}
		}
	}
	var out json.RawMessage
	if err := cl.Call(ctx, op, args, &out); err != nil {
		code := rubiplugin.CodeHomeFailed
		var coded *homeproto.CodedError
		switch {
		case errors.Is(err, homeproto.ErrOffline):
			code = rubiplugin.CodeHomeOffline
		case err.Error() == homeproto.NotPairedMessage:
			code = rubiplugin.CodeHomeNotPaired
		case errors.As(err, &coded) && coded.Code == homeproto.CodeNotAvailableHere:
			code = rubiplugin.CodeHomeNotAvailable
		}
		return nil, &rubiplugin.Error{Code: code, Message: dev.Name + ": " + err.Error()}
	}
	return out, nil
}

// botSudoResult is the result of a hook installation where no one can be asked for the administrator password on that
// computer: the Bot runs one command with sudo. Core passes on exactly one shape and nothing else, with no prefix matching:
//
//	{"/usr/bin/sudo", "--", "/bin/sh", "-c", SCRIPT}   with sha256(SCRIPT) equal to the hash the user approved
//
// SCRIPT is the plan's own script (the same text pkexec would run); Rubi Home's binary is not in the command, so nothing the
// user can write is run as root. A command with another program, more arguments, or a script that does not have the approved
// hash is never passed on.
func (c *Core) botSudoResult(dev *vault.Device, approved string, res map[string]any) (any, error) {
	raw, _ := res["argv"].([]any)
	var argv []string
	for _, a := range raw {
		s, ok := a.(string)
		if !ok {
			return nil, errors.New("Rubi Home gave a command that can't be passed on")
		}
		argv = append(argv, s)
	}
	sum := sha256.Sum256([]byte(lastOf(argv)))
	if !sha256Hex.MatchString(approved) || len(argv) != 5 || argv[0] != "/usr/bin/sudo" || argv[1] != "--" || argv[2] != "/bin/sh" ||
		argv[3] != "-c" || len(argv[4]) == 0 || len(argv[4]) > 512<<10 || hex.EncodeToString(sum[:]) != approved {
		c.Audit.Record("device.hook.failed", audit.Fields{"device": dev.ID, "reason": "refused"})
		return nil, errors.New("Rubi Home gave a command for another installation than the one you approved")
	}
	c.Audit.Record("device.hook.started", audit.Fields{"device": dev.ID})
	return map[string]any{"started": false, "needs_bot_sudo": true, "plan_sha256": approved, "argv": argv,
		"receipt": "Nobody can be asked for the administrator password on " + dev.Name + ". Run the argument list in argv there as it is, and nothing else with administrator rights; its last element is the script whose SHA-256 you approved: " + approved}, nil
}

func lastOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}
