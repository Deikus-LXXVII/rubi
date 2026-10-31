// Package roles is Rubi Home's store of agent roles (docs/design/agent-roles.md, sections 2, 3, 4.2, 6.2 and 7).
//
// A role is a named profile on one project. Home keeps every role as immutable numbered versions
// (<dir>/<id>/versions/<n>.json, 0600), the content of ad-hoc skills as content-addressed blobs, and, for each
// version, a materialized directory that a VM mounts read-only:
//
//	<dir>/<id>/index.json            a cache of the current version (rebuilt from versions/ when stale)
//	<dir>/<id>/versions/<n>.json     the role record of version n (never rewritten)
//	<dir>/<id>/blobs/<sha256>        skill text, named by the SHA-256 Home computed itself
//	<dir>/<id>/content/v<n>/         role.json, plugin/.claude-plugin/plugin.json, plugin/skills/<name>/SKILL.md,
//	                                 plugin/agents/<name>.md
//	<dir>/<id>/content/current       relative symlink "v<n>", swapped atomically
//	<dir>/<id>/claude/               the role's future /rubi-claude mount (empty here)
//
// Put is the only write. It validates the whole spec, scans every skill with sdk/skillscan, and commits in an order
// that a crash can't leave half done: blobs, then content/v<n> (built aside and renamed), then versions/<n>.json
// (the commit point, linked into place so it can never replace another file), then the current symlink, then
// index.json. Open and every Put reconcile a role from the highest versions/<n>.json.
package roles

import (
	"fmt"
	"strings"

	"github.com/Deikus-LXXVII/rubi/sdk/skillscan"
)

// Limits (structural; the per-Bot limits are the plugin's).
const (
	KeepVersions    = 5 // materialized versions kept besides the pinned ones
	MaxTotalDefault = 50
	MaxVersions     = 1000
	MaxDescription  = 300
	MaxInstructions = 8 << 10
	MaxPrompt       = 16 << 10
	MaxSkills       = 20
	MaxAdhocSkills  = 10
	MaxAgents       = 8
	MaxDeny         = 64
	MaxMCP          = 16
	MaxPlugins      = 8
	MaxHosts        = 32
	MaxSSH          = 16
	MaxRoleJSON     = 64 << 10
	MaxDiffLines    = 200 // unified diff lines kept per skill
	MaxDiffBytes    = 16 << 10
	MaxHistory      = 100
)

// Skill sources and states.
const (
	SourceAdhoc   = "adhoc"
	SourceLibrary = "library"
	StateActive   = "active"
	StateHeld     = "held"
)

// Role is one version of a role (section 2.1). Fields marked "set by Home" are overwritten on Put whatever the
// spec says.
type Role struct {
	ID                  string   `json:"id"`                 // set by Home
	Project             string   `json:"project"`            // an agent project id on this device
	DirHash             string   `json:"dir_hash,omitempty"` // set by Home: sha256 of the project's real directory when the role was made
	Name                string   `json:"name"`
	Description         string   `json:"description"`
	Status              string   `json:"status"`  // active or archived
	Version             int      `json:"version"` // set by Home
	Parent              int      `json:"parent"`  // set by Home
	Instructions        string   `json:"instructions"`
	ProjectInstructions bool     `json:"project_instructions"`
	Skills              []Skill  `json:"skills"`
	Agents              []Agent  `json:"agents"`
	Tools               []string `json:"tools"`
	Deny                []Deny   `json:"deny"`
	MCP                 []MCP    `json:"mcp"`
	Plugins             []Plugin `json:"plugins"`
	Hosts               []string `json:"hosts"`
	SSH                 []string `json:"ssh"`
	DefaultMode         string   `json:"default_mode"`
	CreatedBy           string   `json:"created_by"` // set by Home
	CreatedAt           string   `json:"created_at"` // set by Home
	ChangedBy           string   `json:"changed_by"` // set by Home
	ChangedAt           string   `json:"changed_at"` // set by Home
	Change              *Change  `json:"change"`     // from the put's change, clipped
}

// Skill is one skill of a role. For source "adhoc" SHA256 names the blob; for "library" the library fields apply
// (and the signature must verify). Scan and State are set by Home.
type Skill struct {
	Name      string            `json:"name"`
	Source    string            `json:"source"`
	LibraryID string            `json:"library_id,omitempty"`
	Pin       string            `json:"pin,omitempty"`      // "" = follow the latest admitted
	Resolved  string            `json:"resolved,omitempty"` // the library version materialized
	SHA256    string            `json:"sha256"`
	Sig       string            `json:"sig,omitempty"`
	AddedBy   string            `json:"added_by,omitempty"`
	Accepted  []string          `json:"accepted,omitempty"` // flag rule ids the user acknowledged (ad-hoc)
	Scan      *skillscan.Result `json:"scan,omitempty"`     // set by Home
	State     string            `json:"state,omitempty"`    // set by Home: active or held
}

// Agent is a subagent definition; Home rebuilds the markdown file from these fields only.
type Agent struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Prompt      string   `json:"prompt"`
	Tools       []string `json:"tools"`
	Model       string   `json:"model"`
}

// Deny is a deny rule such as Bash(git push:*); By says who set it ("user" or a Bot id).
type Deny struct {
	Rule string `json:"rule"`
	By   string `json:"by,omitempty"`
}

// MCP is an MCP server. It needs the id of the approval that allowed it.
type MCP struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"` // http, sse or stdio
	URL      string   `json:"url,omitempty"`
	Command  string   `json:"command,omitempty"`
	Args     []string `json:"args,omitempty"`
	Approval string   `json:"approval"`
}

// Plugin is a Claude plugin with code (tier 2). It is recorded; the runner doesn't load it yet.
type Plugin struct {
	Name     string `json:"name"`
	Source   string `json:"source"`
	Approval string `json:"approval"`
}

// Change describes what a version changed (the short history kept in each version).
type Change struct {
	Op       string `json:"op,omitempty"`
	Tier     int    `json:"tier,omitempty"`
	Approval string `json:"approval,omitempty"`
	Summary  string `json:"summary,omitempty"`
}

// Row is one line of role.list.
type Row struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Version     int    `json:"version"`
	ChangedBy   string `json:"changed_by,omitempty"`
	ChangedAt   string `json:"changed_at,omitempty"`
	Legacy      bool   `json:"legacy,omitempty"`
}

// PutArgs are the arguments of role.put. ID is empty to create a role (BaseVersion 0). By is the Bot or user the
// plugin acts for; Change is recorded in the version.
type PutArgs struct {
	ID          string            `json:"id"`
	BaseVersion int               `json:"base_version"`
	Spec        *Role             `json:"spec"`
	Blobs       map[string]string `json:"blobs"`
	By          string            `json:"by"`
	Change      *Change           `json:"change"`
}

// SkillState is a skill's outcome in a PutResult.
type SkillState struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Verdict string `json:"verdict"`
}

// PutResult is the answer of role.put.
type PutResult struct {
	ID      string       `json:"id"`
	Version int          `json:"version"`
	Skills  []SkillState `json:"skills"`
}

// ValidationError lists every reason a spec was refused.
type ValidationError struct{ Reasons []string }

func (e *ValidationError) Error() string { return "role refused: " + strings.Join(e.Reasons, "; ") }

// ConflictError is a base_version mismatch.
type ConflictError struct{ Base, Current int }

func (e *ConflictError) Error() string {
	return fmt.Sprintf("the role changed: base_version %d, current version %d", e.Base, e.Current)
}

// LibrarySkill is what a Verifier checks.
type LibrarySkill struct {
	ID       string
	Resolved string
	SHA256   string
	Sig      string
}

// Verifier says whether a library skill's signature is valid. Until the library exists (R8) Refuse is used.
type Verifier interface {
	VerifyLibrary(LibrarySkill) error
}

// Refuse is the stub Verifier: it refuses every library skill.
type Refuse struct{}

// VerifyLibrary always fails.
func (Refuse) VerifyLibrary(LibrarySkill) error {
	return fmt.Errorf("the skill library isn't available yet")
}
