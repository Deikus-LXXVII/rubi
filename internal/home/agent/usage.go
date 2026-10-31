package agent

// Usage modes, per Bot: presets for how much one Bot may run at once, so the owner trades quota for speed
// with one switch per Bot. The limits of a Bot apply to the sessions it started and their subagents only;
// a Bot on light never caps another on medium. A Bot not listed has the default mode; so do the sessions
// the user starts from the panel (no Bot). Raw settings override: the per-project limit and the subagent
// cap for every Bot, and a cap on the whole computer so the machine stays bounded whatever the modes say.

// Usage modes.
const (
	ModeMaximum = "maximum"
	ModeMedium  = "medium" // the default
	ModeLight   = "light"
)

// UsageModes lists the modes, most generous first.
var UsageModes = []string{ModeMaximum, ModeMedium, ModeLight}

// Defaults and bounds.
const (
	DefaultMaxSessionsPerProject = 1
	DefaultMaxSessions           = 4 // on the whole computer, whatever the modes allow (maximum is bounded only by this)
	MaxBots                      = 256
)

type preset struct{ sessions, subagents int }

var presets = map[string]preset{
	ModeMaximum: {0, 0}, // no cap on sessions or subagents
	ModeMedium:  {4, 4},
	ModeLight:   {1, 2},
}

// UsageConfig is what the configuration says (nil or unset values mean the defaults).
type UsageConfig struct {
	Default               string            // mode of a Bot not in Bots, and of the user's own sessions
	Bots                  map[string]string // Bot id -> mode
	MaxSessionsPerProject int               // < 1: 1
	MaxSessions           *int              // on the computer; 0 = no cap, nil or negative: DefaultMaxSessions
	MaxSubagents          *int              // per session, overrides every mode; 0 = no cap, nil or negative: the mode's
}

// Limits are the limits of one Bot. Zero Sessions or Subagents means no cap.
type Limits struct {
	Mode       string
	PerProject int
	Sessions   int // top-level sessions this Bot may run at once
	Subagents  int // per session
	Custom     bool
}

// ModeOf is the mode of a Bot ("" is the user's own sessions); an unknown mode is medium.
func (c UsageConfig) ModeOf(bot string) string {
	if m, ok := c.Bots[bot]; ok && bot != "" && validMode(m) {
		return m
	}
	if validMode(c.Default) {
		return c.Default
	}
	return ModeMedium
}

// For resolves the limits of a Bot.
func (c UsageConfig) For(bot string) Limits {
	mode := c.ModeOf(bot)
	pr := presets[mode]
	l := Limits{Mode: mode, PerProject: DefaultMaxSessionsPerProject, Sessions: pr.sessions, Subagents: pr.subagents}
	if c.MaxSessionsPerProject >= 1 {
		l.PerProject = c.MaxSessionsPerProject
		l.Custom = l.PerProject != DefaultMaxSessionsPerProject
	}
	if c.MaxSubagents != nil && *c.MaxSubagents >= 0 {
		l.Custom = l.Custom || *c.MaxSubagents != pr.subagents
		l.Subagents = *c.MaxSubagents
	}
	return l
}

// Machine is the cap on top-level sessions on the computer (0 = none).
func (c UsageConfig) Machine() int {
	if c.MaxSessions == nil || *c.MaxSessions < 0 {
		return DefaultMaxSessions
	}
	return *c.MaxSessions
}

func validMode(s string) bool { _, ok := presets[s]; return ok }

// UsageChange is the user's change of a mode: one Bot's, the default's, or everyone's (all Bots known and
// the default).
type UsageChange struct {
	Mode    string
	Bot     string
	Default bool
	All     bool
}
