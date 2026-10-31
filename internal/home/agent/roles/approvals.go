package roles

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Home's side of the tiers (design sections 2.2 and 6). The plugin decides who may widen a role and asks the user;
// Home can't tell a Bot from the panel, so it enforces only what holds whoever is calling:
//
//   - a version that ADDS anything wider than the previous version (an MCP server, a Claude plugin, a host, a tool
//     outside SafeTools, an accepted skill flag) names an approval id for each addition: the entry's own `approval`
//     for an MCP server or plugin, the put's change.approval for the rest;
//   - an approval id is spent by the version that commits with it: it can't authorize a second version, a changed
//     entry or another entry. The spent ids are kept in used.json (rebuilt from the version files);
//   - what a put didn't change keeps its labels from the previous version (an MCP server's or plugin's approval,
//     a deny rule's `by`, a skill's `added_by` and accepted flags), so a put can't relabel history. A new deny rule
//     or skill is labelled with the put's own `by`.
//
// Home does not check that an id is a real, pending or granted approval: that is the plugin's (like perm.ask).

// approvalUse records which version spent an approval id and what it authorized.
type approvalUse struct {
	Version int      `json:"version"`
	Keys    []string `json:"keys"`
}

// addition is one thing a version adds, with the approval id that must cover it ("" when none was given).
type addition struct {
	Key      string
	Approval string
}

func digest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

func mcpDigest(m MCP) string {
	return digest(m.Type, m.URL, m.Command, fmt.Sprint(len(m.Args)), strings.Join(m.Args, "\x00"))
}

func pluginDigest(p Plugin) string { return digest(p.Source) }

// additions lists what r adds to prev (nil: nothing before), in a stable order.
func additions(prev, r *Role) []addition {
	if prev == nil {
		prev = &Role{}
	}
	var out []addition
	change := ""
	if r.Change != nil {
		change = r.Change.Approval
	}
	oldMCP := map[string]string{}
	for _, m := range prev.MCP {
		oldMCP[m.Name] = mcpDigest(m)
	}
	for _, m := range r.MCP {
		if d := mcpDigest(m); oldMCP[m.Name] != d {
			out = append(out, addition{"mcp:" + m.Name + ":" + d, m.Approval})
		}
	}
	oldPl := map[string]string{}
	for _, p := range prev.Plugins {
		oldPl[p.Name] = pluginDigest(p)
	}
	for _, p := range r.Plugins {
		if d := pluginDigest(p); oldPl[p.Name] != d {
			out = append(out, addition{"plugin:" + p.Name + ":" + d, p.Approval})
		}
	}
	for _, h := range r.Hosts {
		if !inList(prev.Hosts, h) {
			out = append(out, addition{"host:" + h, change})
		}
	}
	for _, t := range r.Tools {
		if !inList(prev.Tools, t) && !inList(SafeTools, t) {
			out = append(out, addition{"tool:" + t, change})
		}
	}
	oldSkill := map[string]Skill{}
	for _, sk := range prev.Skills {
		oldSkill[sk.Name] = sk
	}
	for _, sk := range r.Skills {
		var had []string
		if o, ok := oldSkill[sk.Name]; ok && o.SHA256 == sk.SHA256 {
			had = o.Accepted
		}
		for _, id := range sk.Accepted {
			if !inList(had, id) {
				out = append(out, addition{"accept:" + sk.Name + ":" + sk.SHA256 + ":" + id, change})
			}
		}
	}
	return out
}

// registerApprovals marks the ids that authorized additions as spent by version v.
func registerApprovals(u *used, v int, adds []addition) {
	by := map[string][]string{}
	for _, a := range adds {
		if a.Approval != "" {
			by[a.Approval] = append(by[a.Approval], a.Key)
		}
	}
	for id, keys := range by {
		sort.Strings(keys)
		u.Approvals[id] = approvalUse{Version: v, Keys: keys}
	}
}

// carry copies the labels of what the put left unchanged from prev, and labels what is new with by.
func carry(r, prev *Role, by string) {
	if prev == nil {
		prev = &Role{}
	}
	oldDeny := map[string]Deny{}
	for _, d := range prev.Deny {
		oldDeny[d.Rule] = d
	}
	for i, d := range r.Deny {
		if o, ok := oldDeny[d.Rule]; ok {
			r.Deny[i].By = o.By
		} else if by != "" {
			r.Deny[i].By = by
		}
	}
	oldMCP := map[string]MCP{}
	for _, m := range prev.MCP {
		oldMCP[m.Name] = m
	}
	for i, m := range r.MCP {
		if o, ok := oldMCP[m.Name]; ok && mcpDigest(o) == mcpDigest(m) {
			r.MCP[i].Approval = o.Approval
		}
	}
	oldPl := map[string]Plugin{}
	for _, p := range prev.Plugins {
		oldPl[p.Name] = p
	}
	for i, p := range r.Plugins {
		if o, ok := oldPl[p.Name]; ok && pluginDigest(o) == pluginDigest(p) {
			r.Plugins[i].Approval = o.Approval
		}
	}
	oldSkill := map[string]Skill{}
	for _, sk := range prev.Skills {
		oldSkill[sk.Name] = sk
	}
	for i, sk := range r.Skills {
		o, ok := oldSkill[sk.Name]
		switch {
		case !ok:
			if by != "" {
				r.Skills[i].AddedBy = by
			}
		default:
			r.Skills[i].AddedBy = o.AddedBy
			if o.SHA256 == sk.SHA256 && sk.Source == SourceAdhoc {
				acc := append([]string{}, o.Accepted...)
				for _, id := range sk.Accepted {
					if !inList(acc, id) {
						acc = append(acc, id)
					}
				}
				sort.Strings(acc)
				if len(acc) > 0 {
					r.Skills[i].Accepted = acc
				}
			}
		}
	}
}

// checkApprovals refuses additions without an approval id, and ids that were already spent.
func checkApprovals(r, prev *Role, u *used, p *problems) {
	adds := additions(prev, r)
	spent := map[string]bool{}
	for _, a := range adds {
		key := a.Key
		if i := strings.IndexByte(key, ':'); i > 0 {
			key = key[:i] + " " + clipText(key[i+1:], 48)
		}
		switch {
		case a.Approval == "":
			p.addf("adding %s needs an approval id", key)
		case u.Approvals[a.Approval].Version > 0:
			if !spent[a.Approval] {
				p.addf("approval %s was already used by version %d", clipText(a.Approval, 64), u.Approvals[a.Approval].Version)
				spent[a.Approval] = true
			}
		}
	}
}
