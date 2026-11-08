package agentsandbox

// Binding is written by Rubi, not accepted from a T3 prompt or provider environment.
// The hash pins a role to the directory it was created for, even if project IDs shift.
type Binding struct {
	HomeDir, Role, Project, DirHash, Provider string
}

// Installation is owner-controlled installation metadata, never discovered via the agent's PATH.
// Missing values are errors; there is no unconfined fallback.
type Installation struct {
	AgentExec   string              `json:"agent_exec"`
	SRT         string              `json:"srt"`
	SearchPath  []string            `json:"search_path"`
	RuntimeRead []string            `json:"runtime_read"`
	Providers   map[string]Provider `json:"providers"`
}

type Provider struct {
	CLI            string   `json:"cli"`
	RuntimeRead    []string `json:"runtime_read"`
	AllowedDomains []string `json:"allowed_domains"`
}

// ProviderRoute describes the upstream setting that actually controls process launch.
type ProviderRoute struct{ Driver, BinaryField string }

// Cursor's current SDK launches in-process and ignores binaryPath for sessions.
// OpenCode needs an authenticated transport across srt's network boundary.
// Neither driver is exposed until its actual execution boundary is connected.
var ProviderRoutes = []ProviderRoute{
	{"claudeAgent", "binaryPath"}, {"codex", "binaryPath"},
	{"grok", "binaryPath"}, {"fx", "binaryPath"}, {"opencode", ""},
	{"antigravity", "binaryPath"}, {"kiro", "binaryPath"}, {"pi", "binaryPath"},
	{"muse", "binaryPath"}, {"acpRegistry", "commandPath"}, {"cursor", ""},
}

func Route(driver string) (ProviderRoute, bool) {
	for _, r := range ProviderRoutes {
		if r.Driver == driver {
			return r, r.BinaryField != ""
		}
	}
	return ProviderRoute{}, false
}

// RouteBlocker explains why an installed CLI is not enough to enable a driver.
func RouteBlocker(driver string) string {
	switch driver {
	case "cursor":
		return "Cursor executes inside the shared T3 server; a CLI shim cannot confine its file tools (requires an isolated T3 server or an upstream execution hook)"
	case "opencode":
		return "OpenCode requires an authenticated private HTTP bridge across the srt network boundary; a binaryPath shim alone is not a safe connection"
	default:
		return "provider has no supported sandbox launch hook"
	}
}
