package t3vm

import (
	"encoding/json"
	"errors"
	"strings"
)

// HookTimeoutSeconds is the hook's own timeout in the managed settings; the hook script gives up a little earlier
// and the gate earlier still (a person may take minutes to answer, the manager's wait is below all three).
const HookTimeoutSeconds = 300

// HookCommand is what Claude Code runs for every tool call.
const HookCommand = "node " + HookVMPath

// ManagedSettings renders /etc/claude-code/managed-settings.json for a session VM (findings F4 and F5): only the
// managed hook runs (a project's or user's settings can't add or turn off hooks), the one PreToolUse hook sends
// every tool call to Rubi Home, and the shared sign-in folder can't be read or written through the file tools or
// named in a Bash command. The deny rules are best effort against a prompt-injected agent; the VM's mounts and the
// proxy are the boundary.
func ManagedSettings(signInDir string) ([]byte, error) {
	if err := checkPath("the sign-in directory", signInDir); err != nil {
		return nil, err
	}
	if strings.ContainsAny(signInDir, "()*?[]") {
		return nil, errors.New("t3vm: the sign-in directory has characters that a permission rule would read as a pattern")
	}
	// A path rule starting with // is an absolute path.
	glob := "/" + signInDir + "/**"
	deny := []string{
		"Read(" + glob + ")", "Read(/" + signInDir + ")",
		"Edit(" + glob + ")", "Edit(/" + signInDir + ")",
		"Bash(*" + signInDir + "*)",
	}
	return json.MarshalIndent(map[string]any{
		"allowManagedHooksOnly": true,
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": "*",
				"hooks":   []any{map[string]any{"type": "command", "command": HookCommand, "timeout": HookTimeoutSeconds}},
			}},
		},
		"permissions": map[string]any{"deny": deny},
	}, "", "  ")
}
