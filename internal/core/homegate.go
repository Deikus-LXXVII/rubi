package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	kindSSHTarget   = "claude-code.ssh_target"
	kindSkillAccept = "claude-code.role_skill_accept"

	// attestedKindsField is the role.put argument core owns: id -> approval kind of every id it attested.
	attestedKindsField = "attested_kinds"

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
//
// For a role.put it also returns the arguments to forward: any `attested_kinds` the plugin sent is dropped and
// replaced by core's own map from each attested id to its approval kind, which Home uses to keep an id from covering
// more than its kind allows (roles/approvals.go).
func (c *Core) attestHomeCall(plugin, op string, args json.RawMessage) (json.RawMessage, error) {
	switch op {
	case opRolePut:
		// encoding/json matches keys ignoring case (Unicode folding too: the Kelvin sign is a k), and which of two
		// such keys wins depends on their order. Core checks one reading and forwards the original, so two keys that
		// fold together anywhere would let core and Home read different specs or kinds.
		if err := noFoldedDuplicates(args); err != nil {
			return nil, errors.New("Rubi Home refused: bad role.put arguments")
		}
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
			return nil, errors.New("Rubi Home refused: bad role.put arguments")
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
		kinds := map[string]string{}
		for _, id := range ids {
			if id == "" {
				continue
			}
			kind, err := c.checkApprovalID(plugin, id, roleApprovalKinds)
			if err != nil {
				return nil, err
			}
			kinds[id] = kind
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(args, &top); err != nil {
			return nil, errors.New("Rubi Home refused: bad role.put arguments")
		}
		raw, err := json.Marshal(kinds)
		if err != nil {
			return nil, errors.New("Rubi Home refused: bad role.put arguments")
		}
		for k := range top {
			if strings.EqualFold(k, attestedKindsField) {
				delete(top, k) // the plugin's own, in any spelling
			}
		}
		top[attestedKindsField] = raw
		out, err := json.Marshal(top)
		if err != nil {
			return nil, errors.New("Rubi Home refused: bad role.put arguments")
		}
		return out, nil
	case opSSHRegister:
		if err := noFoldedDuplicates(args); err != nil {
			return nil, errors.New("Rubi Home refused: bad ssh.register arguments")
		}
		return args, c.attestSSHRegister(plugin, args)
	}
	return args, nil
}

// checkApprovalID accepts an id that is (a) a core approval of this plugin, of a role kind, that is executing or
// executed, or (b) the approval_id of a role_skill_accept approval of this plugin that is executing right now (the
// plugin makes that id itself; it is not a core approval id).
//
// Executing or Executed is reached only through the approving button: a decline ends in Denied, and every role kind
// has one option, the approving one (apply, accept). So any option that gets here is an approval. The id is not
// bound to the content of the change (the plugin's ticket holds the role's fields and core does not parse them);
// Home spends an id once, and the plugin's own pending record ties an id to one change.
//
// It returns the kind the id was attested as.
func (c *Core) checkApprovalID(plugin, id string, kinds map[string]bool) (string, error) {
	if s, err := c.Approvals.Get(id); err == nil && s.Integration == plugin && kinds[s.Kind] &&
		(s.State == approvals.Executing || s.State == approvals.Executed) {
		return s.Kind, nil
	}
	c.execMu.Lock()
	defer c.execMu.Unlock()
	for _, g := range c.execs[plugin] {
		var p struct {
			ApprovalID string `json:"approval_id"`
		}
		if g.kind == kindSkillAccept && kinds[g.kind] && json.Unmarshal(g.payload, &p) == nil && p.ApprovalID != "" && p.ApprovalID == id {
			return g.kind, nil
		}
	}
	return "", fmt.Errorf("Rubi refused: approval %q is not an approval the user gave this plugin for a role change", clip(id, 40))
}

// sshTarget is every field Home's ssh.register takes (sshtarget.RegisterArgs) and the plugin's approval payload
// carries (same JSON names). The gate decodes the arguments strictly into it and compares all of it, so a field
// Home learns later is refused until the gate knows it.
type sshTarget struct {
	Name            string   `json:"name"`
	Host            string   `json:"host"`
	Port            int      `json:"port"`
	User            string   `json:"user"`
	Fingerprint     string   `json:"fingerprint"`
	AllowedCommands []string `json:"allowed_commands"`
	AskEachTime     bool     `json:"ask_each_time"`
	Reuse           bool     `json:"reuse"`
}

func (a sshTarget) same(b sshTarget) bool {
	if a.Name != b.Name || a.Host != b.Host || a.Port != b.Port || a.User != b.User || a.Fingerprint != b.Fingerprint ||
		a.AskEachTime != b.AskEachTime || a.Reuse != b.Reuse {
		return false
	}
	// A set: order and repeats don't matter, a missing list equals an empty one.
	set := func(l []string) map[string]bool {
		m := map[string]bool{}
		for _, x := range l {
			m[x] = true
		}
		return m
	}
	x, y := set(a.AllowedCommands), set(b.AllowedCommands)
	if len(x) != len(y) {
		return false
	}
	for k := range x {
		if !y[k] {
			return false
		}
	}
	return true
}

func (c *Core) attestSSHRegister(plugin string, args json.RawMessage) error {
	var want sshTarget
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&want); err != nil || dec.More() {
		return errors.New("Rubi Home refused: bad ssh.register arguments")
	}
	c.execMu.Lock()
	defer c.execMu.Unlock()
	for _, g := range c.execs[plugin] {
		if g.kind != kindSSHTarget {
			continue
		}
		var got sshTarget // the payload has more fields (device, key_type); they are not Home's
		if json.Unmarshal(g.payload, &got) == nil && got.same(want) && want.Name != "" && want.Fingerprint != "" {
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

// noFoldedDuplicates refuses JSON with an object, at any depth, holding two keys that are equal ignoring case the way
// encoding/json compares them (strings.EqualFold: Unicode simple folding).
func noFoldedDuplicates(raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := foldCheckValue(dec, 0); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

func foldCheckValue(dec *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		var keys []string
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			k, ok := kt.(string)
			if !ok {
				return errors.New("bad key")
			}
			for _, prev := range keys {
				if strings.EqualFold(prev, k) {
					return fmt.Errorf("duplicate key %q", clip(k, 40))
				}
			}
			keys = append(keys, k)
			if err := foldCheckValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token() // }
		return err
	case json.Delim('['):
		for dec.More() {
			if err := foldCheckValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token() // ]
		return err
	}
	return nil
}
