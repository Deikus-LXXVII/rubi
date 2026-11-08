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
	CLI                string            `json:"cli"`
	RuntimeRead        []string          `json:"runtime_read"`
	AllowedDomains     []string          `json:"allowed_domains"`
	CredentialsFile    string            `json:"credentials_file,omitempty"`
	ProjectCredentials map[string]string `json:"project_credentials,omitempty"`
}

// ProviderRoute describes the upstream setting that actually controls process launch.
type ProviderRoute struct{ Driver, BinaryField string }

// Cursor's current SDK launches in-process and ignores binaryPath for sessions.
// Cursor is routed by Rubi to a whole project server, never a shared-server shim.
// OpenCode's shim owns an authenticated inherited-pipe bridge to its sandbox.
var ProviderRoutes = []ProviderRoute{
	{"claudeAgent", "binaryPath"}, {"codex", "binaryPath"},
	{"grok", "binaryPath"}, {"fx", "binaryPath"}, {"opencode", "binaryPath"},
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
		return "Cursor cannot use a CLI shim in the shared T3 server; select provider cursor through Rubi to use its sandboxed project T3 server"
	default:
		return "provider has no supported sandbox launch hook"
	}
}
