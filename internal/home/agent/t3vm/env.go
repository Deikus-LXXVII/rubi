package t3vm

import (
	"errors"
	"strings"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// What the VM's claude gets from T3's environment (finding F1): T3 hands its own environment (a user's API key, the
// OAuth token of a desktop app, SSH_AUTH_SOCK, PATH of the Mac) to every provider process; the VM sees none of it.
//
// The names that pass are listed one by one, not matched by prefix: CLAUDE_CODE_* includes switches that change how
// commands run (CLAUDE_CODE_SHELL_PREFIX), which provider is used (CLAUDE_CODE_USE_*) and what claude trusts
// (CLAUDE_CODE_SIMPLE). These are the ones the Agent SDK or T3 set for a session and that are harmless there:
// who started claude, the SDK's version, the tool-summary and checkpoint switches, the ask-a-question tool, startup
// reporting, and two switches that only turn background features off.
var (
	allowedEnv = map[string]bool{
		"CLAUDE_CODE_ENTRYPOINT":                    true,
		"CLAUDE_AGENT_SDK_VERSION":                  true,
		"CLAUDE_AGENT_SDK_MCP_NO_PREFIX":            true,
		"CLAUDE_CODE_EMIT_TOOL_USE_SUMMARIES":       true,
		"CLAUDE_CODE_EMIT_STARTUP_TIMING":           true,
		"CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING": true,
		"CLAUDE_CODE_ENABLE_ASK_USER_QUESTION_TOOL": true,
		"CLAUDE_CODE_STARTUP_FAILURE_RESULTS":       true,
		"CLAUDE_CODE_EAGER_FLUSH":                   true,
		"CLAUDE_CODE_DISABLE_CRON":                  true,
		"CLAUDE_CODE_DISABLE_TERMINAL_TITLE":        true,
		"T3_AGENT_THREAD":                           true,
	}
	// secretWords: a name with one of these is dropped, wherever it sits in the name, even if the list above grew
	// to include it. Besides credentials and identities, this drops what would change where requests go or how
	// commands run (another provider, a base URL, a proxy, a shell prefix wrapped around every command, a helper
	// that prints a key).
	secretWords = []string{"TOKEN", "OAUTH", "ACCOUNT", "ORGANIZATION", "HOST_AUTH", "API_KEY", "HELPER", "CREDENTIAL",
		"EMAIL", "MESSAGING", "BASE_URL", "PROXY", "SHELL_PREFIX", "SECRET", "PASSWORD"}
	// providerSwitch: CLAUDE_CODE_USE_BEDROCK, _VERTEX, _FOUNDRY... send the session to another provider.
	providerSwitch = "CLAUDE_CODE_USE_"
	// imageEnv are the image's own telemetry switches: T3 can't turn them on.
	imageEnv = map[string]bool{"DISABLE_TELEMETRY": true, "DISABLE_ERROR_REPORTING": true, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": true}
)

// FilterEnv keeps, from T3's environment, only the names in the list above (and not the image's switches or a
// credential-shaped name). Home sets everything else itself (SessionSpec.EnvLines).
func FilterEnv(t3 map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range t3 {
		if !allowedEnv[k] || imageEnv[k] || strings.ContainsAny(v, "\n\r\x00") {
			continue
		}
		up := strings.ToUpper(k)
		secret := strings.HasPrefix(up, providerSwitch)
		for _, w := range secretWords {
			secret = secret || strings.Contains(up, w)
		}
		if !secret {
			out[k] = v
		}
	}
	return out
}

// EnvLines is the session's environment file (KEY=value lines, sorted): the filtered T3 environment, then what
// Home sets and T3 cannot change: the role's config directory, the shared sign-in folder, and the proxy variables
// (HTTPS_PROXY, NO_PROXY; proxy holds only those two, they carry the proxy's credential and the file is 0600).
func (s SessionSpec) EnvLines(t3 map[string]string, proxy map[string]string) ([]byte, error) {
	if s.ClaudeDir == "" || s.SignInDir == "" {
		return nil, errors.New("t3vm: the environment needs the claude and sign-in directories")
	}
	env := FilterEnv(t3)
	for k, v := range proxy {
		if k != "HTTPS_PROXY" && k != "NO_PROXY" {
			return nil, errors.New("t3vm: only HTTPS_PROXY and NO_PROXY may come from the proxy")
		}
		env[k] = v
	}
	env["CLAUDE_CONFIG_DIR"] = s.ClaudeDir
	env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] = s.SignInDir
	return agent.EnvFile(env)
}
