package roles

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Deikus-LXXVII/rubi/sdk/skillscan"
)

// SafeTools are the built-in tools a Bot may give a role at tier 1 (section 6.1). KnownTools are all the names
// Home accepts in a role's tools. THESE LISTS MUST MATCH THE RUNNER'S known list (runner/, R1): a name here that
// the runner doesn't know makes the runner refuse session.start for the role.
var (
	SafeTools  = []string{"Read", "Edit", "Write", "Glob", "Grep", "Bash", "Agent", "Skill", "WebFetch", "WebSearch"}
	KnownTools = append(append([]string{}, SafeTools...),
		"CronCreate", "CronDelete", "CronList", "ScheduleWakeup", "Monitor", "Workflow", "SendMessage",
		"EnterWorktree", "ExitWorktree", "TaskStop", "ToolSearch", "NotebookEdit")
)

// Models a subagent may name, and the modes a role may default to (dontAsk isn't offered; section 6.3).
var (
	models = map[string]bool{"inherit": true, "sonnet": true, "haiku": true, "opus": true}
	modes  = map[string]bool{"plan": true, "default": true, "acceptEdits": true, "auto": true, "bypassPermissions": true}
)

var (
	idRE       = regexp.MustCompile(`^r_[a-z0-9]{8}$`)
	projectRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	nameRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	itemRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`) // skills, subagents, library ids
	shaRE      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	numRE      = regexp.MustCompile(`^[0-9]{1,6}$`)
	sigRE      = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{1,200}$`)
	approvalRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	whoRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,63}$`)
	ruleRE     = regexp.MustCompile(`^[a-z]{3}\.[a-z0-9]{1,16}$`)
	denyRE     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}(\([^\x00-\x1f\x7f]{1,200}\))?$`)
	hostRE     = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	sshRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

// IsID says whether s has the shape of a role id (the only strings ever used in a path).
func IsID(s string) bool { return idRE.MatchString(s) }

type problems []string

func (p *problems) addf(format string, a ...any) {
	if len(*p) < 20 {
		*p = append(*p, fmt.Sprintf(format, a...))
	}
}

// text checks a text value: UTF-8, no NUL or control characters (newlines and tabs only when multi), at most max
// bytes.
func (p *problems) text(field, s string, max int, multi bool) {
	if len(s) > max {
		p.addf("%s is over %d bytes", field, max)
		return
	}
	if !utf8.ValidString(s) {
		p.addf("%s isn't UTF-8", field)
		return
	}
	for _, r := range s {
		if r == 0x7f || (r >= 0x80 && r < 0xa0) || (r < 0x20 && !(multi && (r == '\n' || r == '\t' || r == '\r'))) {
			p.addf("%s has a control character", field)
			return
		}
		if skillscan.IsInvisible(r) {
			p.addf("%s has an invisible character (U+%04X)", field, r)
			return
		}
	}
}

// scanText refuses a free text that the skill scanner would reject or flag. A skill with a flag can be held until
// someone accepts the flag; instructions, prompts and descriptions have no such state, so any hit refuses them (the
// way out is to reword, or to add the host to the role's hosts, which is a widening that needs its own approval).
func (p *problems) scanText(field, s string, hosts []string) {
	if s == "" || !utf8.ValidString(s) {
		return
	}
	hits := skillscan.ScanText(s, hosts)
	if len(hits) == 0 {
		return
	}
	var ids []string
	for _, h := range hits {
		if len(ids) < 4 {
			ids = append(ids, fmt.Sprintf("%s (line %d)", h.ID, h.Line))
		}
	}
	p.addf("%s refused by the text scan: %s", field, strings.Join(ids, ", "))
}

// DirHash is the binding of a role to a project directory: the SHA-256 of the directory's path ("" for no directory).
func DirHash(dir string) string {
	if dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(filepath.Clean(dir)))
	return hex.EncodeToString(sum[:])
}

func inList(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func clipText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

// normalize checks spec and returns the canonical record (lists sorted, defaults filled, server fields cleared).
// It does not touch the disk. blobs maps sha256 to text for every skill; known says which of them already exist.
func (s *Store) normalize(spec *Role, cur *Role) (*Role, problems) {
	var p problems
	r := &Role{}
	if spec == nil {
		p.addf("the spec is missing")
		return r, p
	}
	r.Project = spec.Project
	if cur != nil {
		if r.Project == "" {
			r.Project = cur.Project
		}
		if r.Project != cur.Project {
			p.addf("a role can't move to another project")
		}
	}
	if !projectRE.MatchString(r.Project) {
		p.addf("project: bad id")
	} else if s.opt.Projects != nil && !inList(s.opt.Projects(), r.Project) {
		p.addf("project: unknown project %q", clipText(r.Project, 64))
	}
	// The binding of the role to its project's directory is Home's: kept from the previous version, else taken from
	// the project id Home resolves, never from the spec.
	switch {
	case cur != nil && cur.DirHash != "":
		r.DirHash = cur.DirHash
	case s.opt.ProjectDir != nil && projectRE.MatchString(r.Project):
		r.DirHash = DirHash(s.opt.ProjectDir(r.Project))
	}
	r.Name, r.Description = spec.Name, spec.Description
	switch {
	case !nameRE.MatchString(r.Name):
		p.addf("name must match [a-z0-9][a-z0-9-]{0,31}")
	case r.Name == "default":
		p.addf("name \"default\" is the project's legacy role")
	}
	p.text("description", r.Description, MaxDescription, false)
	r.Status = spec.Status
	if r.Status == "" {
		r.Status = "active"
	}
	if r.Status != "active" && r.Status != "archived" {
		p.addf("status must be active or archived")
	}
	r.Instructions, r.ProjectInstructions = spec.Instructions, spec.ProjectInstructions
	p.text("instructions", r.Instructions, MaxInstructions, true)
	r.DefaultMode = spec.DefaultMode
	if r.DefaultMode == "" {
		r.DefaultMode = "default"
	}
	if !modes[r.DefaultMode] {
		p.addf("default_mode %q isn't offered for roles", clipText(r.DefaultMode, 32))
	}

	// Tools.
	r.Tools = []string{}
	seen := map[string]bool{}
	for _, t := range spec.Tools {
		switch {
		case !inList(KnownTools, t):
			p.addf("tools: unknown tool %q", clipText(t, 64))
		case seen[t]:
			p.addf("tools: %s is listed twice", t)
		default:
			seen[t] = true
			r.Tools = append(r.Tools, t)
		}
	}
	sort.Strings(r.Tools)

	// Deny rules.
	r.Deny = []Deny{}
	seen = map[string]bool{}
	if len(spec.Deny) > MaxDeny {
		p.addf("deny: more than %d rules", MaxDeny)
	}
	for _, d := range spec.Deny {
		switch {
		case !denyRE.MatchString(d.Rule):
			p.addf("deny: bad rule %q", clipText(d.Rule, 64))
		case seen[d.Rule]:
			p.addf("deny: %q is listed twice", clipText(d.Rule, 64))
		case d.By != "" && !whoRE.MatchString(d.By):
			p.addf("deny: bad \"by\"")
		default:
			seen[d.Rule] = true
			r.Deny = append(r.Deny, d)
		}
	}
	sort.Slice(r.Deny, func(i, j int) bool { return r.Deny[i].Rule < r.Deny[j].Rule })

	// MCP servers: only with the id of the approval that allowed them.
	r.MCP = []MCP{}
	seen = map[string]bool{}
	if len(spec.MCP) > MaxMCP {
		p.addf("mcp: more than %d servers", MaxMCP)
	}
	for _, m := range spec.MCP {
		n := clipText(m.Name, 32)
		switch {
		case !nameRE.MatchString(m.Name):
			p.addf("mcp %q: bad name", n)
			continue
		case seen[m.Name]:
			p.addf("mcp %q is listed twice", n)
			continue
		case !approvalRE.MatchString(m.Approval):
			p.addf("mcp %q: it needs an approval id", n)
			continue
		}
		seen[m.Name] = true
		switch m.Type {
		case "http", "sse":
			u, err := url.Parse(m.URL)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(m.URL) > 500 || m.Command != "" || len(m.Args) > 0 {
				p.addf("mcp %q: an https url without credentials is needed (and no command)", n)
			}
			p.text("mcp url", m.URL, 500, false)
		case "stdio":
			if m.Command == "" || m.URL != "" || len(m.Args) > 16 {
				p.addf("mcp %q: a command (at most 16 args) is needed (and no url)", n)
			}
			p.text("mcp command", m.Command, 200, false)
			for _, a := range m.Args {
				p.text("mcp arg", a, 200, false)
			}
		default:
			p.addf("mcp %q: type must be http, sse or stdio", n)
		}
		r.MCP = append(r.MCP, m)
	}
	sort.Slice(r.MCP, func(i, j int) bool { return r.MCP[i].Name < r.MCP[j].Name })

	// Claude plugins.
	r.Plugins = []Plugin{}
	seen = map[string]bool{}
	if len(spec.Plugins) > MaxPlugins {
		p.addf("plugins: more than %d", MaxPlugins)
	}
	for _, pl := range spec.Plugins {
		n := clipText(pl.Name, 32)
		switch {
		case !nameRE.MatchString(pl.Name):
			p.addf("plugin %q: bad name", n)
		case seen[pl.Name]:
			p.addf("plugin %q is listed twice", n)
		case !approvalRE.MatchString(pl.Approval):
			p.addf("plugin %q: it needs an approval id", n)
		case pl.Source == "" || len(pl.Source) > 200:
			p.addf("plugin %q: a source is needed", n)
		default:
			p.text("plugin source", pl.Source, 200, false)
			seen[pl.Name] = true
			r.Plugins = append(r.Plugins, pl)
		}
	}
	sort.Slice(r.Plugins, func(i, j int) bool { return r.Plugins[i].Name < r.Plugins[j].Name })

	// Hosts and SSH targets.
	r.Hosts = []string{}
	seen = map[string]bool{}
	if len(spec.Hosts) > MaxHosts {
		p.addf("hosts: more than %d", MaxHosts)
	}
	for _, h := range spec.Hosts {
		h = strings.ToLower(h)
		if len(h) > 253 || !hostRE.MatchString(h) {
			p.addf("hosts: bad host %q", clipText(h, 64))
		} else if !seen[h] {
			seen[h] = true
			r.Hosts = append(r.Hosts, h)
		}
	}
	sort.Strings(r.Hosts)
	r.SSH = []string{}
	seen = map[string]bool{}
	if len(spec.SSH) > MaxSSH {
		p.addf("ssh: more than %d targets", MaxSSH)
	}
	for _, t := range spec.SSH {
		if !sshRE.MatchString(t) {
			p.addf("ssh: bad target name %q", clipText(t, 32))
		} else if !seen[t] {
			seen[t] = true
			r.SSH = append(r.SSH, t)
		}
	}
	sort.Strings(r.SSH)

	// Subagents: rebuilt from fields, tools never wider than the role's.
	r.Agents = []Agent{}
	seen = map[string]bool{}
	if len(spec.Agents) > MaxAgents {
		p.addf("agents: more than %d", MaxAgents)
	}
	for _, a := range spec.Agents {
		n := clipText(a.Name, 48)
		if !itemRE.MatchString(a.Name) {
			p.addf("agent %q: bad name", n)
			continue
		}
		if seen[a.Name] {
			p.addf("agent %q is listed twice", n)
			continue
		}
		seen[a.Name] = true
		if strings.TrimSpace(a.Description) == "" {
			p.addf("agent %q: a description is needed", n)
		}
		p.text("agent description", a.Description, MaxDescription, false)
		if strings.TrimSpace(a.Prompt) == "" {
			p.addf("agent %q: a prompt is needed", n)
		}
		p.text("agent prompt", a.Prompt, MaxPrompt, true)
		if a.Model == "" {
			a.Model = "inherit"
		}
		if !models[a.Model] {
			p.addf("agent %q: model must be inherit, sonnet, haiku or opus", n)
		}
		var tools []string
		dup := map[string]bool{}
		for _, t := range a.Tools {
			switch {
			case !inList(KnownTools, t):
				p.addf("agent %q: unknown tool %q", n, clipText(t, 64))
			case dup[t]:
			case inList(r.Tools, t):
				dup[t] = true
				tools = append(tools, t)
			default:
				dup[t] = true // dropped: a subagent can't have a tool its role lacks
			}
		}
		if len(a.Tools) > 0 && len(tools) == 0 {
			p.addf("agent %q: none of its tools is in the role's tools", n)
		}
		a.Tools = tools
		if a.Tools == nil {
			a.Tools = []string{}
		}
		r.Agents = append(r.Agents, a)
	}
	sort.Slice(r.Agents, func(i, j int) bool { return r.Agents[i].Name < r.Agents[j].Name })

	// The free texts: hidden text, secrets, exfiltration shapes and override or widening claims are refused.
	scanHosts := append(append([]string{}, r.Hosts...), s.baseHosts()...)
	p.scanText("description", r.Description, scanHosts)
	p.scanText("instructions", r.Instructions, scanHosts)
	for _, a := range r.Agents {
		n := clipText(a.Name, 48)
		p.scanText("agent "+n+" description", a.Description, scanHosts)
		p.scanText("agent "+n+" prompt", a.Prompt, scanHosts)
	}

	// Skills: the shape here; the content, scan and signature in checkSkills.
	r.Skills = []Skill{}
	seen = map[string]bool{}
	adhoc := 0
	if len(spec.Skills) > MaxSkills {
		p.addf("skills: more than %d", MaxSkills)
	}
	for _, sk := range spec.Skills {
		n := clipText(sk.Name, 48)
		if !itemRE.MatchString(sk.Name) {
			p.addf("skill %q: bad name", n)
			continue
		}
		if seen[sk.Name] {
			p.addf("skill %q is listed twice", n)
			continue
		}
		seen[sk.Name] = true
		if !shaRE.MatchString(sk.SHA256) {
			p.addf("skill %q: sha256 must be 64 lowercase hex characters", n)
		}
		if sk.AddedBy != "" && !whoRE.MatchString(sk.AddedBy) {
			p.addf("skill %q: bad added_by", n)
		}
		out := Skill{Name: sk.Name, Source: sk.Source, SHA256: sk.SHA256, AddedBy: sk.AddedBy}
		switch sk.Source {
		case SourceAdhoc:
			adhoc++
			if sk.LibraryID != "" || sk.Sig != "" || sk.Pin != "" || sk.Resolved != "" {
				p.addf("skill %q: library fields on an ad-hoc skill", n)
			}
			for _, id := range sk.Accepted {
				if !ruleRE.MatchString(id) {
					p.addf("skill %q: bad accepted rule id", n)
					break
				}
			}
			if len(sk.Accepted) > 16 {
				p.addf("skill %q: too many accepted rules", n)
			} else {
				out.Accepted = append([]string(nil), sk.Accepted...)
				sort.Strings(out.Accepted)
			}
		case SourceLibrary:
			if !itemRE.MatchString(sk.LibraryID) || !numRE.MatchString(sk.Resolved) || (sk.Pin != "" && !numRE.MatchString(sk.Pin)) {
				p.addf("skill %q: a library skill needs a valid library_id and resolved version", n)
			}
			if !sigRE.MatchString(sk.Sig) {
				p.addf("skill %q: a library skill needs a signature", n)
			}
			out.LibraryID, out.Pin, out.Resolved, out.Sig = sk.LibraryID, sk.Pin, sk.Resolved, sk.Sig
		default:
			p.addf("skill %q: source must be adhoc or library", n)
		}
		r.Skills = append(r.Skills, out)
	}
	if adhoc > MaxAdhocSkills {
		p.addf("skills: more than %d ad-hoc skills", MaxAdhocSkills)
	}
	sort.Slice(r.Skills, func(i, j int) bool { return r.Skills[i].Name < r.Skills[j].Name })
	return r, p
}

// checkSkills verifies each skill against its content: the hash, the signature (library) and the scan. A reject
// is a reason; a flag holds the skill unless every flagged rule was accepted.
func (s *Store) checkSkills(r *Role, content map[string][]byte, p *problems) {
	hosts := append(append([]string{}, r.Hosts...), s.baseHosts()...)
	for i := range r.Skills {
		sk := &r.Skills[i]
		n := clipText(sk.Name, 48)
		raw, ok := content[sk.SHA256]
		if !ok {
			p.addf("skill %q: its content (sha256 %.12s) wasn't sent and isn't stored", n, sk.SHA256)
			continue
		}
		if sk.Source == SourceLibrary {
			err := s.opt.Verifier.VerifyLibrary(LibrarySkill{ID: sk.LibraryID, Resolved: sk.Resolved, SHA256: sk.SHA256, Sig: sk.Sig})
			if err != nil {
				p.addf("skill %q: library signature refused: %s", n, clipText(err.Error(), 120))
				continue
			}
		}
		res := skillscan.Scan(sk.Name, raw, hosts)
		sk.Scan = &res
		switch res.Verdict {
		case skillscan.Reject:
			var ids []string
			for _, h := range res.Rules {
				if h.Severity == skillscan.Reject && len(ids) < 5 {
					ids = append(ids, fmt.Sprintf("%s (line %d)", h.ID, h.Line))
				}
			}
			p.addf("skill %q rejected by the scan: %s", n, strings.Join(ids, ", "))
		case skillscan.Flag:
			sk.State = StateHeld
			if sk.Source == SourceLibrary {
				sk.State = StateActive // the curator acknowledged the flags at admission
			} else if acceptedAll(res, sk.Accepted) {
				sk.State = StateActive
			}
		default:
			sk.State = StateActive
		}
		if sk.Source == SourceAdhoc {
			sk.Accepted = flaggedIn(res, sk.Accepted) // keep only what was actually flagged
		}
	}
}

func flagIDs(res skillscan.Result) []string {
	var ids []string
	for _, h := range res.Rules {
		if h.Severity == skillscan.Flag && !inList(ids, h.ID) {
			ids = append(ids, h.ID)
		}
	}
	return ids
}

func acceptedAll(res skillscan.Result, accepted []string) bool {
	for _, id := range flagIDs(res) {
		if !inList(accepted, id) {
			return false
		}
	}
	return true
}

func flaggedIn(res skillscan.Result, accepted []string) []string {
	var out []string
	for _, id := range flagIDs(res) {
		if inList(accepted, id) {
			out = append(out, id)
		}
	}
	return out
}
