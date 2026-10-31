package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
)

// Core vouches for the approvals Rubi Home relies on.
//
// Rubi Home checks no approval itself: a role.put names an approval id for what it widens, and ssh.register is
// "approved" only because the plugin says so. Home can't tell a plugin from core (both arrive through the same
// relay), so core checks at the relay, the only way a plugin reaches Home (the same reasoning as library.key in
// library.go). Before it forwards claude-agent.role.put or claude-agent.ssh.register it requires that
//
//   - every approval id in a role.put is a real approval of this very plugin, of a kind that may widen a role
//     (roleApprovalKinds), that the user is deciding on or has decided (executing or executed). A made-up, foreign,
//     denied, expired or wrong-kind id is refused. Spending an id once is Home's job (roles/used.json);
//   - an ssh.register matches the payload of an approval of kind claude-code.ssh_target that is being executed for
//     this plugin right now (host, port, user, name and the host key fingerprint the user confirmed).
//
// The capability itself is reserved for the official plugin (plugins.ReservedHomeCapability), so these gates are
// what stands between the official plugin's bugs and Home, and the reserved list is what keeps others out.

const (
	kindSSHTarget = "claude-code.ssh_target"

	opRolePut     = "claude-agent.role.put"
	opSSHRegister = "claude-agent.ssh.register"
)

// roleApprovalKinds are the approval kinds whose ids a role.put may name.
var roleApprovalKinds = map[string]bool{
	"claude-code.role_widen":        true,
	"claude-code.role_foreign":      true,
	"claude-code.role_skill_accept": true,
}

// execGrant is an approval of a plugin whose execute is running: the user has decided it.
type execGrant struct {
	kind    string
	payload json.RawMessage
}

// beginExec records that plugin's execute for an approval of kind is running with payload; the returned func ends it.
func (c *Core) beginExec(plugin, kind string, payload json.RawMessage) func() {
	g := &execGrant{kind: kind, payload: payload}
	c.execMu.Lock()
	if c.execs == nil {
		c.execs = map[string][]*execGrant{}
	}
	c.execs[plugin] = append(c.execs[plugin], g)
	c.execMu.Unlock()
	return func() {
		c.execMu.Lock()
		defer c.execMu.Unlock()
		list := c.execs[plugin]
		for i, x := range list {
			if x == g {
				c.execs[plugin] = append(list[:i:i], list[i+1:]...)
				break
			}
		}
		if len(c.execs[plugin]) == 0 {
			delete(c.execs, plugin)
		}
	}
}

// attestHomeCall refuses a Home call that carries approvals core can't vouch for.
func (c *Core) attestHomeCall(plugin, op string, args json.RawMessage) error {
	switch op {
	case opRolePut:
		var in struct {
			Spec struct {
				MCP []struct {
					Approval string `json:"approval"`
				} `json:"mcp"`
				Plugins []struct {
					Approval string `json:"approval"`
				} `json:"plugins"`
			} `json:"spec"`
			Change *struct {
				Approval string `json:"approval"`
			} `json:"change"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return errors.New("Rubi Home refused: bad role.put arguments")
		}
		var ids []string
		for _, m := range in.Spec.MCP {
			ids = append(ids, m.Approval)
		}
		for _, p := range in.Spec.Plugins {
			ids = append(ids, p.Approval)
		}
		if in.Change != nil {
			ids = append(ids, in.Change.Approval)
		}
		for _, id := range ids {
			if id == "" {
				continue
			}
			if err := c.checkApprovalID(plugin, id, roleApprovalKinds); err != nil {
				return err
			}
		}
	case opSSHRegister:
		return c.attestSSHRegister(plugin, args)
	}
	return nil
}

func (c *Core) checkApprovalID(plugin, id string, kinds map[string]bool) error {
	s, err := c.Approvals.Get(id)
	if err != nil || s.Integration != plugin || !kinds[s.Kind] ||
		(s.State != approvals.Executing && s.State != approvals.Executed) {
		return fmt.Errorf("Rubi refused: approval %q is not an approval the user gave this plugin for a role change", clip(id, 40))
	}
	return nil
}

func (c *Core) attestSSHRegister(plugin string, args json.RawMessage) error {
	type target struct {
		Name        string `json:"name"`
		Host        string `json:"host"`
		Port        int    `json:"port"`
		User        string `json:"user"`
		Fingerprint string `json:"fingerprint"`
	}
	var want target
	if err := json.Unmarshal(args, &want); err != nil {
		return errors.New("Rubi Home refused: bad ssh.register arguments")
	}
	c.execMu.Lock()
	defer c.execMu.Unlock()
	for _, g := range c.execs[plugin] {
		if g.kind != kindSSHTarget {
			continue
		}
		var got target
		if json.Unmarshal(g.payload, &got) == nil && got == want && want.Name != "" && want.Fingerprint != "" {
			return nil
		}
	}
	return errors.New("Rubi refused: no approval from the user is being executed for registering this SSH target")
}

func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 {
			return ' '
		}
		return r
	}, s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
