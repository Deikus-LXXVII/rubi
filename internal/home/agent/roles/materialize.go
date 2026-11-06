package roles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/sdk/skillscan"
)

// roleJSON is the file the runner reads at each query start (section 4.2). Only active skills are listed.
type roleJSON struct {
	Role                string              `json:"role"`
	Version             int                 `json:"version"`
	Skills              []string            `json:"skills"`
	Tools               []string            `json:"tools"`
	Deny                []string            `json:"deny"`
	MCP                 map[string]mcpEntry `json:"mcp"`
	Instructions        string              `json:"instructions"`
	ProjectInstructions bool                `json:"project_instructions"`
	DefaultMode         string              `json:"default_mode"`
}

type mcpEntry struct {
	Type    string   `json:"type"`
	URL     string   `json:"url,omitempty"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
}

func marshal(v any, indent bool) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	_ = enc.Encode(v)
	return b.Bytes()
}

// RoleJSON renders role.json for r.
func RoleJSON(r *Role) []byte {
	j := roleJSON{Role: r.ID, Version: r.Version, Skills: []string{}, Tools: append([]string{}, r.Tools...), Deny: []string{},
		MCP: map[string]mcpEntry{}, Instructions: r.Instructions, ProjectInstructions: r.ProjectInstructions, DefaultMode: r.DefaultMode}
	for _, s := range r.Skills {
		if s.State == StateActive {
			j.Skills = append(j.Skills, s.Name)
		}
	}
	for _, d := range r.Deny {
		j.Deny = append(j.Deny, d.Rule)
	}
	for _, m := range r.MCP {
		j.MCP[m.Name] = mcpEntry{Type: m.Type, URL: m.URL, Command: m.Command, Args: m.Args}
	}
	return marshal(j, false)
}

// PluginJSON renders .claude-plugin/plugin.json: the plugin is always named "role".
func PluginJSON(r *Role) []byte {
	return marshal(struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Description string `json:"description"`
	}{"role", fmt.Sprintf("%d.0.0", r.Version), "Rubi role " + r.ID}, true)
}

// AgentMD rebuilds a subagent's file from its fields only (no mcpServers, hooks or skills). The tools were already
// intersected with the role's tools by normalize; an empty list leaves the line out (the subagent has the role's
// base set, which it can't exceed).
func AgentMD(a Agent) []byte {
	var b strings.Builder
	b.WriteString("---\nname: " + skillscan.QuoteYAML(a.Name) + "\ndescription: " + skillscan.QuoteYAML(a.Description) + "\n")
	if len(a.Tools) > 0 {
		b.WriteString("tools: " + strings.Join(a.Tools, ", ") + "\n")
	}
	b.WriteString("model: " + a.Model + "\n---\n")
	b.WriteString(a.Prompt)
	if !strings.HasSuffix(a.Prompt, "\n") {
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// writeTree writes a file (creating parents) with sync. Content is world-readable on purpose: a VM user reads it
// through a read-only mount; the helper directory above it is private.
func writeTree(root, rel string, data []byte) error {
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := writeTemp(filepath.Dir(p), data, 0o644)
	if err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// materialize builds content/v<n> for r beside its final name and renames it into place. raw gives a skill's
// stored text by hash. A directory that is already there (an orphan of a crash) is replaced.
//
// A new version (strict) is refused when a skill is rejected by the scan; a version rebuilt from disk (not strict)
// was accepted when it was stored, so a scanner that has since become stricter must not leave the role unusable:
// the skill is logged and left out of the directory and of role.json, and a put or revert that removes it works.
func (s *Store) materialize(r *Role, strict bool, raw func(sha string) ([]byte, error)) error {
	content := filepath.Join(s.roleDir(r.ID), "content")
	if err := os.MkdirAll(content, 0o755); err != nil {
		return err
	}
	tmp := tmpName(content, ".tmp-v")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(tmp)
		}
	}()
	// The skills first: which of them stay decides what role.json lists.
	rr := *r
	rr.Skills = append([]Skill(nil), r.Skills...)
	texts := map[string][]byte{}
	for i, sk := range rr.Skills {
		if sk.State != StateActive {
			continue
		}
		if s.revoked(sk) { // withdrawn from the library: never in a role that is built from now on
			s.logf("role %s version %d: library skill %q (%s@%s) was withdrawn and is left out", r.ID, r.Version, sk.Name, sk.LibraryID, sk.Resolved)
			rr.Skills[i].State = StateHeld
			continue
		}
		text, err := raw(sk.SHA256)
		if err != nil {
			return err
		}
		res := skillscan.Scan(sk.Name, text, nil)
		if res.Verdict == skillscan.Reject {
			if strict {
				return fmt.Errorf("skill %q is rejected by the scan", sk.Name)
			}
			s.logf("role %s version %d: skill %q is rejected by %s and is left out", r.ID, r.Version, sk.Name, skillscan.ScannerVersion)
			rr.Skills[i].State = StateHeld
			continue
		}
		texts[sk.Name] = res.Normalized
	}
	rj := RoleJSON(&rr)
	if len(rj) > MaxRoleJSON {
		return fmt.Errorf("role.json would be %d bytes (the limit is %d)", len(rj), MaxRoleJSON)
	}
	if err := writeTree(tmp, "role.json", rj); err != nil {
		return err
	}
	if err := writeTree(tmp, "plugin/.claude-plugin/plugin.json", PluginJSON(r)); err != nil {
		return err
	}
	// An empty skills/ and agents/ exist even with no entries, so the plugin layout is the same for every role.
	for _, d := range []string{"plugin/skills", "plugin/agents"} {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0o755); err != nil {
			return err
		}
	}
	for _, sk := range rr.Skills {
		if t, ok := texts[sk.Name]; ok {
			if err := writeTree(tmp, "plugin/skills/"+sk.Name+"/SKILL.md", t); err != nil {
				return err
			}
		}
	}
	for _, a := range r.Agents {
		if err := writeTree(tmp, "plugin/agents/"+a.Name+".md", AgentMD(a)); err != nil {
			return err
		}
	}
	if err := syncTree(tmp); err != nil {
		return err
	}
	final := filepath.Join(content, fmt.Sprintf("v%d", r.Version))
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	ok = true
	return syncDir(content)
}

// syncTree syncs every directory under root (the files were synced when written).
func syncTree(root string) error {
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return syncDir(p)
		}
		return nil
	})
}

// revoked says whether a skill is a library version that was withdrawn.
func (s *Store) revoked(sk Skill) bool {
	r, ok := s.opt.Verifier.(Revoker)
	return ok && sk.Source == SourceLibrary && r.Revoked(sk.LibraryID, sk.Resolved)
}

// Left is a skill that EnforceRevocations took out of a built role version.
type Left struct {
	Role    string `json:"role"`
	Version int    `json:"version"`
	Skill   string `json:"skill"`
	Library string `json:"library"` // id@version
}

// EnforceRevocations rebuilds, without the library versions that were withdrawn, every materialized version of
// the role that still carries one, and records each skill taken out in revocations.jsonl. Call it when a VM starts
// and when a new revocation list arrives.
func (s *Store) EnforceRevocations(id string) ([]Left, error) {
	if !IsID(id) {
		return nil, errors.New("bad role id")
	}
	if _, ok := s.opt.Verifier.(Revoker); !ok {
		return nil, nil
	}
	unlock := s.lockRole(id)
	defer unlock()
	ents, err := os.ReadDir(filepath.Join(s.roleDir(id), "content"))
	if err != nil {
		return nil, nil
	}
	var left []Left
	var firstErr error
	for _, e := range ents {
		m := contentDirRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		r, err := s.readVersion(id, n)
		if err != nil {
			continue
		}
		var hit []Left
		for _, sk := range r.Skills {
			if sk.State == StateActive && s.revoked(sk) && s.builtWith(id, n, sk.Name) {
				hit = append(hit, Left{Role: id, Version: n, Skill: sk.Name, Library: sk.LibraryID + "@" + sk.Resolved})
			}
		}
		if len(hit) == 0 {
			continue
		}
		if err := s.materialize(r, false, func(sha string) ([]byte, error) { return s.blob(id, sha) }); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		left = append(left, hit...)
	}
	s.auditRevocations(left)
	return left, firstErr
}

// builtWith says whether content/v<n> still has the skill (a rebuilt version no longer has it, so it isn't counted twice).
func (s *Store) builtWith(id string, n int, skill string) bool {
	_, err := os.Stat(filepath.Join(s.roleDir(id), "content", fmt.Sprintf("v%d", n), "plugin", "skills", skill, "SKILL.md"))
	return err == nil
}

func (s *Store) auditRevocations(left []Left) {
	if len(left) == 0 {
		return
	}
	f, err := os.OpenFile(filepath.Join(s.opt.Dir, "revocations.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		s.logf("revocations.jsonl: %v", err)
		return
	}
	defer f.Close()
	at := s.opt.Now().UTC().Format(time.RFC3339)
	for _, l := range left {
		b, _ := json.Marshal(struct {
			At string `json:"at"`
			Left
		}{at, l})
		_, _ = f.Write(append(b, '\n'))
		s.logf("role %s version %d: library skill %q (%s) was withdrawn and taken out", l.Role, l.Version, l.Skill, l.Library)
	}
}
