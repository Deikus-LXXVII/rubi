package t3vm

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Deikus-LXXVII/rubi/internal/t3"
)

// Claude Code's permission modes from the most restricted to the least; plan only reads, so it is below all of them
// and passes through whatever the ceiling.
var claudeModes = []string{"default", "acceptEdits", "auto", "bypassPermissions"}

// PermissionMode is Claude Code's name for a T3 runtime mode (an unknown one counts as the most restricted).
func PermissionMode(m t3.Mode) string {
	switch m {
	case t3.AutoAcceptEdits:
		return "acceptEdits"
	case t3.Auto:
		return "auto"
	case t3.FullAccess:
		return "bypassPermissions"
	}
	return "default"
}

func modeRank(m string) (int, bool) {
	for i, k := range claudeModes {
		if k == m {
			return i, true
		}
	}
	return 0, false
}

// RefusedError says which argument of T3's command line Home won't run.
type RefusedError struct {
	Arg    string
	Reason string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("Rubi won't start claude with %q: %s", e.Arg, e.Reason)
}

// Policy is what an argument list is checked against.
type Policy struct {
	// Ceiling is the highest mode the role allows.
	Ceiling t3.Mode
	// Cwd and Attachments are the only directories --add-dir may name (the thread's workspace and T3's attachments
	// folder; both are mounted, so nothing else of the Mac can be added through argv).
	Cwd         string
	Attachments string
	// T3Port is the port of T3's own MCP server, the only MCP endpoint a session may be given.
	T3Port int
}

// ClampedArgv is an argument list that ClampArgv checked: ExecArgs takes nothing else, so T3's argv can't reach
// claude unchecked.
type ClampedArgv struct{ args []string }

// SessionID is the claude session id the arguments start or resume ("" when none).
func (c ClampedArgv) SessionID() string {
	for _, f := range []string{"--session-id", "--resume"} { // a new id wins over the one a fork resumes from
		for i, a := range c.args {
			if a == f && i+1 < len(c.args) {
				return c.args[i+1]
			}
			if v, ok := strings.CutPrefix(a, f+"="); ok {
				return v
			}
		}
	}
	return ""
}

// Args returns a copy of the checked arguments.
func (c ClampedArgv) Args() []string { return append([]string(nil), c.args...) }

// The arguments T3 passes (S0 log, docs/plans/t3-integration.md section 9): flags that take a value, flags that
// don't, and the ones refused by name. Everything else is refused as unknown.
var (
	valueFlags = map[string]bool{
		"--output-format": true, "--input-format": true, "--thinking": true, "--thinking-display": true, "--model": true,
		"--permission-prompt-tool": true, "--allowedTools": true, "--disallowedTools": true, "--tools": true,
		"--permission-mode": true, "--add-dir": true, "--session-id": true, "--resume": true, "--effort": true,
		"--mcp-config": true, "--settings": true,
	}
	boolFlags = map[string]bool{
		"--verbose": true, "--include-partial-messages": true, "--fork-session": true, "--strict-mcp-config": true,
		"--dangerously-skip-permissions": true, "--allow-dangerously-skip-permissions": true,
	}
	// refusedFlags would change where claude reads its policy from, or add settings that could turn the hook off.
	refusedFlags = map[string]string{
		"--setting-sources":        "the setting sources are fixed by Rubi",
		"--plugin-dir":             "plugins can bring hooks",
		"--agents":                 "agent definitions can bring hooks",
		"--bare":                   "bare mode skips the managed settings' hooks",
		"--disable-slash-commands": "not part of a T3 session",
	}
	sessionID = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z_-]{0,127}$`)
)

// ClampArgv checks T3's argument list (the part after the claude binary) against the policy and returns the list to
// run: --permission-mode lowered to the ceiling (added as "default" when absent, as a project's settings could
// otherwise pick the mode), the skip-permissions flags dropped unless the ceiling is full access, and an error for
// anything Home doesn't know T3 to need, so a new T3 flag fails loudly instead of running unchecked.
func ClampArgv(argv []string, p Policy) (ClampedArgv, error) {
	ceil, _ := modeRank(PermissionMode(p.Ceiling))
	full := ceil == len(claudeModes)-1
	var out []string
	sawMode, sawStrict := false, false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "--") {
			return ClampedArgv{}, &RefusedError{a, "only options are accepted (the prompt comes on stdin)"}
		}
		name, val, hasVal := strings.Cut(a, "=")
		if why, bad := refusedFlags[name]; bad {
			return ClampedArgv{}, &RefusedError{name, why}
		}
		switch {
		case boolFlags[name]:
			if hasVal {
				return ClampedArgv{}, &RefusedError{a, "this option takes no value"}
			}
			if (name == "--dangerously-skip-permissions" || name == "--allow-dangerously-skip-permissions") && !full {
				continue // dropped: the role's ceiling is below full access
			}
			sawStrict = sawStrict || name == "--strict-mcp-config"
			out = append(out, a)
		case valueFlags[name]:
			if !hasVal {
				if i+1 >= len(argv) {
					return ClampedArgv{}, &RefusedError{name, "the value is missing"}
				}
				i++
				val = argv[i]
			}
			if strings.HasPrefix(val, "-") {
				return ClampedArgv{}, &RefusedError{name, "the value looks like another option"}
			}
			val, err := checkValue(name, val, p)
			if err != nil {
				return ClampedArgv{}, err
			}
			if name == "--permission-mode" {
				sawMode = true
				val = clampMode(val, ceil)
			}
			if hasVal {
				out = append(out, name+"="+val)
			} else {
				out = append(out, name, val)
			}
		default:
			return ClampedArgv{}, &RefusedError{name, "Rubi doesn't know this option; T3 Code may need a newer Rubi"}
		}
	}
	if !sawMode {
		out = append(out, "--permission-mode", "default")
	}
	// Only the MCP servers named on the command line (checked above) are used, never a project's .mcp.json.
	if !sawStrict {
		out = append(out, "--strict-mcp-config")
	}
	return ClampedArgv{out}, nil
}

func clampMode(m string, ceil int) string {
	if m == "plan" {
		return m
	}
	r, ok := modeRank(m)
	if !ok || r > ceil {
		if !ok {
			return claudeModes[0]
		}
		return claudeModes[ceil]
	}
	return m
}

func checkValue(name, val string, p Policy) (string, error) {
	switch name {
	case "--output-format", "--input-format":
		if val != "stream-json" {
			return "", &RefusedError{name, "only stream-json is used"}
		}
	case "--permission-prompt-tool":
		if val != "stdio" {
			return "", &RefusedError{name, "approvals are answered over stdio only"}
		}
	case "--session-id", "--resume":
		if !sessionID.MatchString(val) {
			return "", &RefusedError{name, "not a session id"}
		}
	case "--add-dir":
		d := filepath.Clean(val)
		if d != p.Cwd && (p.Attachments == "" || d != p.Attachments) {
			return "", &RefusedError{name, "only the thread's folder and T3's attachments folder may be added"}
		}
		return d, nil
	case "--settings":
		if err := checkSettings(name, val); err != nil {
			return "", err
		}
	case "--allowedTools":
		// T3's own tools only: a pre-approval must not widen what a call may do (Rubi's gate decides every call).
		if val != t3Tools {
			return "", &RefusedError{name, "only T3's own tools may be pre-approved"}
		}
	case "--mcp-config":
		if err := checkMCP(name, val, p.T3Port); err != nil {
			return "", err
		}
	}
	return val, nil
}

// settingsKeys are the only settings T3 may pass on the command line (it passes {"showThinkingSummaries":true}). The
// managed settings win over them anyway, but a setting Rubi has not looked at has no place here, and a file path
// would name something outside the mounts.
var settingsKeys = map[string]bool{"showThinkingSummaries": true}

func checkSettings(flag, val string) error {
	var cfg map[string]json.RawMessage
	if !strings.HasPrefix(strings.TrimSpace(val), "{") || json.Unmarshal([]byte(val), &cfg) != nil {
		return &RefusedError{flag, "only inline JSON settings are accepted, not a file"}
	}
	for k, v := range cfg {
		var b bool
		if !settingsKeys[k] || json.Unmarshal(v, &b) != nil {
			return &RefusedError{flag, "the setting " + k + " can't be given on the command line"}
		}
	}
	return nil
}

// t3Tools is the pre-approval T3 passes for its own MCP server.
const t3Tools = "mcp__t3-code__*"

// checkMCP accepts an inline {"mcpServers": {name: {type: http|sse, url: http://127.0.0.1:<T3 port>/..., headers}}}:
// a file path would name something outside the mounts, and a server with a command would run in the VM under
// Rubi's nose; the only endpoint that is reachable from the VM and meant for it is T3's own.
func checkMCP(flag, val string, port int) error {
	bad := func(why string) error { return &RefusedError{flag, why} }
	var cfg map[string]json.RawMessage
	if !strings.HasPrefix(strings.TrimSpace(val), "{") || json.Unmarshal([]byte(val), &cfg) != nil {
		return bad("only an inline JSON configuration is accepted, not a file")
	}
	for k := range cfg {
		if k != "mcpServers" {
			return bad("only mcpServers may be configured")
		}
	}
	var servers map[string]map[string]json.RawMessage
	if json.Unmarshal(cfg["mcpServers"], &servers) != nil {
		return bad("mcpServers is not a set of servers")
	}
	if port <= 0 {
		return bad("this session has no T3 port, so no MCP server may be given")
	}
	for name, srv := range servers {
		for k := range srv {
			if k != "type" && k != "url" && k != "headers" {
				return bad("server " + name + " has " + k + ": only type, url and headers are accepted")
			}
		}
		var typ, rawURL string
		if json.Unmarshal(srv["type"], &typ) != nil || (typ != "http" && typ != "sse") {
			return bad("server " + name + " must be an http or sse server")
		}
		if json.Unmarshal(srv["url"], &rawURL) != nil {
			return bad("server " + name + " has no url")
		}
		u, err := url.Parse(rawURL)
		if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() != strconv.Itoa(port) || u.User != nil {
			return bad("server " + name + " must be at http://127.0.0.1:" + strconv.Itoa(port) + " (T3's own server)")
		}
	}
	return nil
}

// ExtraDirs are the read-only directories a clamped argv needs mounted: its --add-dir values besides the workspace.
func ExtraDirs(c ClampedArgv, cwd string) []string {
	argv := c.args
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(argv); i++ {
		var d string
		if argv[i] == "--add-dir" && i+1 < len(argv) {
			d = argv[i+1]
		} else if v, ok := strings.CutPrefix(argv[i], "--add-dir="); ok {
			d = v
		}
		if d != "" && d != cwd && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}
