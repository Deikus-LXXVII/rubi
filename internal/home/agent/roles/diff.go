package roles

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// Entry is one structural difference. Kind is added, removed or changed; Key names the list item.
type Entry struct {
	Field string `json:"field"`
	Key   string `json:"key,omitempty"`
	Kind  string `json:"kind"`
	From  any    `json:"from,omitempty"`
	To    any    `json:"to,omitempty"`
}

// SkillText is the clipped unified diff of one skill's text.
type SkillText struct {
	Name    string `json:"name"`
	From    string `json:"from_sha256,omitempty"`
	To      string `json:"to_sha256,omitempty"`
	Diff    string `json:"diff"`
	Clipped bool   `json:"clipped,omitempty"`
}

// DiffResult is the answer of role.diff.
type DiffResult struct {
	ID        string      `json:"id"`
	From      int         `json:"from"`
	To        int         `json:"to"`
	Changes   []Entry     `json:"changes"`
	SkillText []SkillText `json:"skill_text"`
}

// Diff compares two versions of a role.
func (s *Store) Diff(id string, from, to int) (*DiffResult, error) {
	if _, err := s.current(id); err != nil {
		return nil, err
	}
	a, err := s.readVersion(id, from)
	if err != nil {
		return nil, err
	}
	b, err := s.readVersion(id, to)
	if err != nil {
		return nil, err
	}
	res := &DiffResult{ID: id, From: from, To: to, Changes: []Entry{}, SkillText: []SkillText{}}
	scalar := func(field string, x, y any) {
		if fmt.Sprint(x) != fmt.Sprint(y) {
			res.Changes = append(res.Changes, Entry{Field: field, Kind: "changed", From: x, To: y})
		}
	}
	scalar("name", a.Name, b.Name)
	scalar("description", a.Description, b.Description)
	scalar("status", a.Status, b.Status)
	scalar("instructions", a.Instructions, b.Instructions)
	scalar("project_instructions", a.ProjectInstructions, b.ProjectInstructions)
	scalar("default_mode", a.DefaultMode, b.DefaultMode)
	set := func(field string, x, y []string) {
		for _, v := range x {
			if !inList(y, v) {
				res.Changes = append(res.Changes, Entry{Field: field, Key: v, Kind: "removed"})
			}
		}
		for _, v := range y {
			if !inList(x, v) {
				res.Changes = append(res.Changes, Entry{Field: field, Key: v, Kind: "added"})
			}
		}
	}
	set("tools", a.Tools, b.Tools)
	set("hosts", a.Hosts, b.Hosts)
	set("ssh", a.SSH, b.SSH)
	keyed := func(field string, x, y map[string]any) {
		for k, v := range x {
			if w, ok := y[k]; !ok {
				res.Changes = append(res.Changes, Entry{Field: field, Key: k, Kind: "removed", From: v})
			} else if !sameJSON(v, w) {
				res.Changes = append(res.Changes, Entry{Field: field, Key: k, Kind: "changed", From: v, To: w})
			}
		}
		for k, w := range y {
			if _, ok := x[k]; !ok {
				res.Changes = append(res.Changes, Entry{Field: field, Key: k, Kind: "added", To: w})
			}
		}
	}
	keyed("deny", denyMap(a), denyMap(b))
	keyed("skills", skillMap(a), skillMap(b))
	keyed("agents", agentMap(a), agentMap(b))
	keyed("mcp", mcpMap(a), mcpMap(b))
	keyed("plugins", pluginMap(a), pluginMap(b))
	sortEntries(res.Changes)

	// Skill text: every skill whose content differs, was added or was removed.
	names := map[string]bool{}
	am, bm := map[string]Skill{}, map[string]Skill{}
	for _, sk := range a.Skills {
		am[sk.Name], names[sk.Name] = sk, true
	}
	for _, sk := range b.Skills {
		bm[sk.Name], names[sk.Name] = sk, true
	}
	for _, n := range sortedKeys(names) {
		x, y := am[n], bm[n]
		if x.SHA256 == y.SHA256 {
			continue
		}
		var xt, yt string
		if x.SHA256 != "" {
			if t, err := s.blob(id, x.SHA256); err == nil {
				xt = string(t)
			}
		}
		if y.SHA256 != "" {
			if t, err := s.blob(id, y.SHA256); err == nil {
				yt = string(t)
			}
		}
		d, clipped := unified(fmt.Sprintf("%s/SKILL.md (v%d)", n, from), fmt.Sprintf("%s/SKILL.md (v%d)", n, to), xt, yt)
		res.SkillText = append(res.SkillText, SkillText{Name: n, From: x.SHA256, To: y.SHA256, Diff: d, Clipped: clipped})
	}
	return res, nil
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func denyMap(r *Role) map[string]any {
	m := map[string]any{}
	for _, d := range r.Deny {
		m[d.Rule] = d
	}
	return m
}
func skillMap(r *Role) map[string]any {
	m := map[string]any{}
	for _, v := range r.Skills {
		v.Scan = nil // the scan result isn't a change of the role
		m[v.Name] = v
	}
	return m
}
func agentMap(r *Role) map[string]any {
	m := map[string]any{}
	for _, v := range r.Agents {
		m[v.Name] = v
	}
	return m
}
func mcpMap(r *Role) map[string]any {
	m := map[string]any{}
	for _, v := range r.MCP {
		m[v.Name] = v
	}
	return m
}
func pluginMap(r *Role) map[string]any {
	m := map[string]any{}
	for _, v := range r.Plugins {
		m[v.Name] = v
	}
	return m
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func sortEntries(es []Entry) {
	for i := 1; i < len(es); i++ {
		for j := i; j > 0 && less(es[j], es[j-1]); j-- {
			es[j], es[j-1] = es[j-1], es[j]
		}
	}
}

func less(a, b Entry) bool {
	if a.Field != b.Field {
		return a.Field < b.Field
	}
	if a.Key != b.Key {
		return a.Key < b.Key
	}
	return a.Kind < b.Kind
}

// unified makes a unified diff with 3 lines of context, clipped to MaxDiffLines lines and MaxDiffBytes bytes.
func unified(aName, bName, a, b string) (string, bool) {
	x, y := splitLines(a), splitLines(b)
	// Longest common subsequence table (skills are at most 800 lines).
	n, m := len(x), len(y)
	if n > 2000 || m > 2000 {
		return "(too long to compare)\n", true
	}
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type op struct {
		c    byte // ' ', '-', '+'
		text string
	}
	var ops []op
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && x[i] == y[j]:
			ops = append(ops, op{' ', x[i]})
			i++
			j++
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, op{'-', x[i]})
			i++
		default:
			ops = append(ops, op{'+', y[j]})
			j++
		}
	}
	const ctx = 3
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", aName, bName)
	lines := 2
	clipped := false
	ai, bi := 1, 1 // line numbers of ops[k] in a and b
	k := 0
	for k < len(ops) {
		if ops[k].c == ' ' {
			ai++
			bi++
			k++
			continue
		}
		start := k
		for start > 0 && ops[start-1].c == ' ' && k-start < ctx {
			start--
		}
		end := k
		for end < len(ops) {
			if ops[end].c != ' ' {
				end++
				continue
			}
			gap := 0
			for e := end; e < len(ops) && ops[e].c == ' '; e++ {
				gap++
			}
			if gap > 2*ctx || end+gap >= len(ops) {
				end += min(gap, ctx)
				break
			}
			end += gap
		}
		ha, hb, na, nb := ai-(k-start), bi-(k-start), 0, 0
		for _, o := range ops[start:end] {
			if o.c != '+' {
				na++
			}
			if o.c != '-' {
				nb++
			}
		}
		hdr := fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", ha, na, hb, nb)
		body := []string{hdr}
		for _, o := range ops[start:end] {
			body = append(body, string(o.c)+o.text+"\n")
		}
		for _, l := range body {
			if lines >= MaxDiffLines || out.Len()+len(l) > MaxDiffBytes {
				clipped = true
				break
			}
			out.WriteString(l)
			lines++
		}
		if clipped {
			break
		}
		for _, o := range ops[k:end] {
			if o.c != '+' {
				ai++
			}
			if o.c != '-' {
				bi++
			}
		}
		k = end
	}
	return out.String(), clipped
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
