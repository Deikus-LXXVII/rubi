package core

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/audit"
	"github.com/Deikus-LXXVII/rubi/internal/events"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// Proving which Bot is calling. Bots on one account share a computer and Rubi's MCP server and name
// themselves, so a name alone proves nothing. What only the real Bot has is its routine webhook: the
// platform delivers a POST there to that Bot alone. So Rubi hands out short-lived codes through webhooks:
// every wake carries the woken Bot's current code, and rubi_verify sends one on request. Reading or
// acknowledging events needs the code of the Bot named.
//
// A request for someone else's code reaches the real Bot, which is told to report it if it didn't ask.
// Wrong codes are counted: after a few, codes for that name are blocked for a while and the administrator
// is warned.

const (
	agentCodeTTL     = 15 * time.Minute
	agentCodeEvery   = 2 * time.Minute // at most one requested code per Bot this often (each costs a wake)
	agentMaxFailures = 3
	agentBlockFor    = 30 * time.Minute
)

type agentAuth struct {
	mu      sync.Mutex
	codes   map[string]agentCode
	asked   map[string]time.Time
	fails   map[string][]time.Time
	blocked map[string]time.Time
}

type agentCode struct {
	code    string
	expires time.Time
}

func agentKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// agentCodeFor returns the Bot's current code, making a new one when it has none or it is about to expire.
func (c *Core) agentCodeFor(name string) string {
	a := &c.agentAuth
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.codes == nil {
		a.codes = map[string]agentCode{}
	}
	k := agentKey(name)
	if cur, ok := a.codes[k]; ok && time.Until(cur.expires) > agentCodeTTL/2 {
		return cur.code
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	code := base64.RawURLEncoding.EncodeToString(b)
	a.codes[k] = agentCode{code: code, expires: time.Now().Add(agentCodeTTL)}
	return code
}

// agentsConnected reports whether any Bot is connected (before that, there is nobody to tell apart).
func (c *Core) agentsConnected() bool {
	n := 0
	_ = c.Vault.View(func(d *vault.Data) error { n = len(d.Agents); return nil })
	return n > 0
}

// ErrAgentCode means the call needs the Bot's code from its webhook.
var ErrAgentCode = errors.New("a code from your webhook is needed")

// checkAgentCode verifies that code is the named Bot's current code.
func (c *Core) checkAgentCode(name, code string) error {
	if !c.agentsConnected() {
		return nil
	}
	if !c.HasAgent(name) {
		return fmt.Errorf("pass agent: your Bot's name as connected to Rubi (agents: %s)", c.agentNames())
	}
	if strings.TrimSpace(code) == "" {
		return ErrAgentCode
	}
	a := &c.agentAuth
	a.mu.Lock()
	k := agentKey(name)
	// The right code always passes, blocked or not. A block answers wrong codes only: otherwise anyone who can
	// name a Bot could lock it out of its own account with three wrong guesses. (Rubi can't tell the callers
	// apart: they share one MCP server, so failures are counted per name, not per caller.)
	cur, ok := a.codes[k]
	if ok && time.Now().Before(cur.expires) && subtle.ConstantTimeCompare([]byte(cur.code), []byte(strings.TrimSpace(code))) == 1 {
		a.mu.Unlock()
		return nil
	}
	if until := a.blocked[k]; time.Now().Before(until) {
		a.mu.Unlock()
		return errors.New("too many wrong codes for this Bot; try again later (a correct code still works)")
	}
	if a.fails == nil {
		a.fails, a.blocked = map[string][]time.Time{}, map[string]time.Time{}
	}
	recent := []time.Time{time.Now()}
	for _, t := range a.fails[k] {
		if time.Since(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	a.fails[k] = recent
	block := len(recent) >= agentMaxFailures
	if block {
		a.blocked[k] = time.Now().Add(agentBlockFor)
		a.fails[k] = nil // the Bot's current code stays valid: a stranger's guesses don't cost it its access
	}
	a.mu.Unlock()
	if block {
		c.agentSuspicious(name, fmt.Sprintf("%d wrong codes in a few minutes", len(recent)))
		return errors.New("too many wrong codes for this Bot; wrong codes are blocked for 30 minutes (the right one still works) and the user is told")
	}
	return errors.New("that code is wrong or expired; codes come with each Rubi webhook (agent_code), or ask with rubi_verify")
}

// RequestAgentCode sends the named Bot its code through its own webhook (rubi_verify).
func (c *Core) RequestAgentCode(name string) error {
	if !c.HasAgent(name) {
		return fmt.Errorf("no agent %q (agents: %s)", name, c.agentNames())
	}
	a := &c.agentAuth
	k := agentKey(name)
	a.mu.Lock()
	if a.asked == nil {
		a.asked = map[string]time.Time{}
	}
	if last, ok := a.asked[k]; ok && time.Since(last) < agentCodeEvery {
		a.mu.Unlock()
		return errors.New("a code was sent to this Bot's webhook a moment ago; use that one")
	}
	a.asked[k] = time.Now()
	a.mu.Unlock()
	var hook *vault.Agent
	_ = c.Vault.View(func(d *vault.Data) error {
		if x := findAgent(d, name); x != nil {
			cp := *x
			hook = &cp
		}
		return nil
	})
	if hook == nil {
		return errors.New("no such agent")
	}
	c.Audit.Record("agent.code_requested", audit.Fields{"agent": hook.Name})
	// Delivered straight to the webhook and never kept in the event list, where others could read it.
	ev := events.Event{ID: "evt_code_" + randomID()[:8], Integration: "rubi", Type: "agent.code", CreatedAt: time.Now().UTC(),
		Data: map[string]any{"agent": hook.Name,
			"message": "A code to read your Rubi events was requested for you. It is in agent_code. If neither you nor the user asked for it just now, tell the user: another Bot may be trying to read Rubi's events under your name."}}
	go c.deliverTo(*hook, ev)
	return nil
}

// agentSuspicious warns the administrator (and records it).
func (c *Core) agentSuspicious(name, why string) {
	c.Audit.Record("agent.suspicious", audit.Fields{"agent": name, "why": why})
	admin := ""
	_ = c.Vault.View(func(d *vault.Data) error {
		if a := defaultAgent(d); a != nil {
			admin = a.Name
		}
		return nil
	})
	c.Events.EmitFor(admin, false, "rubi", "agent.suspicious", map[string]any{"agent": name, "why": why,
		"next_step": "Tell the user: something tried to read Rubi's events as the Bot \"" + name + "\" with wrong codes (" + why + "). If it wasn't them, they should check which Bots and routines run on their account."}, nil)
}

// Tool arguments for telling a plugin which Bot is calling. A Bot names itself with rubi_agent and
// proves it with a code that only its own webhook receives: rubi_agent_code (the agent_code of its latest
// Rubi webhook, or one from rubi_verify) or an access code for this plugin (rubi_access). Rubi then
// passes the plugin rubi_verified_agent, the Bot's registered name (the webhook the user confirmed with
// the passkey). A rubi_verified_agent the caller sends itself is always removed, so a plugin can trust it;
// without a proof the plugin gets none.
const (
	argAgentCode     = "rubi_agent_code"
	argVerifiedAgent = "rubi_verified_agent"
)

// verifyCaller applies the rules above to a tool call's arguments for plugin.
func (c *Core) verifyCaller(plugin string, args json.RawMessage) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(args, &obj) != nil || obj == nil {
		return args, nil // not an object: nothing to check (the plugin rejects it)
	}
	_, spoof := obj[argVerifiedAgent]
	rawCode, hasCode := obj[argAgentCode]
	if !spoof && !hasCode {
		if _, named := obj[AccessAgentArg]; !named {
			return args, nil
		}
	}
	delete(obj, argVerifiedAgent)
	delete(obj, argAgentCode)
	var name, code, access string
	_ = json.Unmarshal(obj[AccessAgentArg], &name)
	_ = json.Unmarshal(rawCode, &code)
	_ = json.Unmarshal(obj[AccessCodeArg], &access)
	if name != "" && (hasCode || access != "") {
		registered, ok := c.webhookAgent(name)
		switch {
		case !ok && hasCode:
			return nil, fmt.Errorf("rubi_agent: no Bot named %q is connected to Rubi with a webhook (agents: %s)", oneLineName(name), c.agentNames())
		case !ok:
		case hasCode:
			if err := c.checkAgentCode(registered, code); err != nil {
				if errors.Is(err, ErrAgentCode) {
					return nil, errors.New("rubi_agent_code: pass the agent_code of your latest Rubi webhook (or call rubi_verify with your Bot name to get one)")
				}
				return nil, err
			}
			obj[argVerifiedAgent], _ = json.Marshal(registered)
		case strings.EqualFold(c.accessAgent(access, plugin), registered):
			obj[argVerifiedAgent], _ = json.Marshal(registered)
		}
	}
	return json.Marshal(obj)
}
