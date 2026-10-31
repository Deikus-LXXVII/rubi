package agent

// Usage modes: presets over the raw limits, so the owner trades quota for speed with one switch. A raw
// setting (agent_max_sessions, agent_max_sessions_per_project, agent_max_subagents_per_session) overrides
// the preset for that limit. The per-project limit is not part of a preset: it stays its own setting.

// Usage modes.
const (
	ModeMaximum = "maximum"
	ModeMedium  = "medium" // the default
	ModeLight   = "light"
)

// UsageModes lists the modes, most generous first.
var UsageModes = []string{ModeMaximum, ModeMedium, ModeLight}

// DefaultMaxSessionsPerProject is the per-project limit unless set.
const DefaultMaxSessionsPerProject = 1

type preset struct{ total, subagents int }

var presets = map[string]preset{
	ModeMaximum: {0, 0}, // no cap on sessions or subagents
	ModeMedium:  {4, 4},
	ModeLight:   {1, 2},
}

// UsageConfig is what the configuration says: the mode and the raw overrides (nil = not set).
type UsageConfig struct {
	Mode                  string
	MaxSessionsPerProject int  // < 1: not set (1)
	MaxSessions           *int // 0 = no cap; negative: not set
	MaxSubagents          *int // per session, 0 = no cap; negative: not set
}

// Limits are the limits in force. Zero Total or Subagents means no cap.
type Limits struct {
	Mode       string
	PerProject int
	Total      int
	Subagents  int
	Custom     bool // a raw setting differs from the mode's preset
}

// ResolveUsage turns the configuration into the limits in force; an unknown mode is medium.
func ResolveUsage(c UsageConfig) Limits {
	mode := c.Mode
	pr, ok := presets[mode]
	if !ok {
		mode, pr = ModeMedium, presets[ModeMedium]
	}
	l := Limits{Mode: mode, PerProject: DefaultMaxSessionsPerProject, Total: pr.total, Subagents: pr.subagents}
	if c.MaxSessionsPerProject >= 1 {
		l.PerProject = c.MaxSessionsPerProject
		l.Custom = l.Custom || l.PerProject != DefaultMaxSessionsPerProject
	}
	if c.MaxSessions != nil && *c.MaxSessions >= 0 {
		l.Custom = l.Custom || *c.MaxSessions != pr.total
		l.Total = *c.MaxSessions
	}
	if c.MaxSubagents != nil && *c.MaxSubagents >= 0 {
		l.Custom = l.Custom || *c.MaxSubagents != pr.subagents
		l.Subagents = *c.MaxSubagents
	}
	return l
}

func validMode(s string) bool { _, ok := presets[s]; return ok }
