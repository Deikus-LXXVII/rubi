package t3vm

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/t3"
)

const (
	tCwd = "/w/proj"
	tAtt = "/t3/userdata/attachments/th-1"
)

const tPort = 4410

func pol(c t3.Mode) Policy { return Policy{Ceiling: c, Cwd: tCwd, Attachments: tAtt, T3Port: tPort} }

// clampRaw is ClampArgv with the result as a plain list, for comparing.
func clampRaw(argv []string, p Policy) ([]string, error) {
	c, err := ClampArgv(argv, p)
	return c.Args(), err
}

// clamp is clampRaw without the --strict-mcp-config that ClampArgv adds when the input has none (tested on its own).
func clamp(argv []string, p Policy) ([]string, error) {
	got, err := clampRaw(argv, p)
	if err == nil && !contains(argv, "--strict-mcp-config") && len(got) > 0 && got[len(got)-1] == "--strict-mcp-config" {
		got = got[:len(got)-1]
	}
	return got, err
}

const t3MCP = `{"mcpServers":{"t3-code":{"type":"http","url":"http://127.0.0.1:4410/mcp","headers":{"Authorization":"Bearer x"}}}}`

// t3Argv is the command line of the S0 wrapper log (docs/plans/t3-integration.md section 9).
func t3Argv(mode string, extra ...string) []string {
	a := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json", "--thinking", "adaptive",
		"--thinking-display", "summarized", "--model", "claude-sonnet", "--permission-prompt-tool", "stdio",
		"--allowedTools", "mcp__t3-code__*", "--tools", "default", "--permission-mode", mode, "--include-partial-messages",
		"--add-dir", tCwd, "--add-dir", tAtt, "--session-id=0b0d0f4e-1111-4222-8333-444455556666"}
	return append(a, extra...)
}

func TestClampArgvPassesWhatT3SendsAndForcesStrictMCP(t *testing.T) {
	for _, extra := range [][]string{nil, {"--resume", "0b0d0f4e-1111-4222-8333-444455556666"}, {"--resume", "abc123", "--fork-session"}, {"--strict-mcp-config"},
		{"--mcp-config", t3MCP, "--strict-mcp-config"}, {"--mcp-config", `{"mcpServers":{"a":{"type":"sse","url":"http://127.0.0.1:4410/sse"}}}`}} {
		in := t3Argv("default", extra...)
		got, err := clampRaw(in, pol(t3.ApprovalRequired))
		if err != nil {
			t.Fatalf("%v: %v", extra, err)
		}
		want := in
		if !contains(in, "--strict-mcp-config") {
			want = append(append([]string(nil), in...), "--strict-mcp-config")
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("changed:\n got %q\nwant %q", got, want)
		}
		if n := count(got, "--strict-mcp-config"); n != 1 {
			t.Fatalf("--strict-mcp-config %d times", n)
		}
	}
}

func contains(a []string, s string) bool { return count(a, s) > 0 }

func count(a []string, s string) (n int) {
	for _, x := range a {
		if x == s {
			n++
		}
	}
	return
}

func TestClampArgvLowersThePermissionModeToTheCeiling(t *testing.T) {
	cases := []struct {
		ceiling t3.Mode
		want    string
		out     string
	}{
		{t3.ApprovalRequired, "bypassPermissions", "default"},
		{t3.ApprovalRequired, "acceptEdits", "default"},
		{t3.ApprovalRequired, "auto", "default"},
		{t3.AutoAcceptEdits, "bypassPermissions", "acceptEdits"},
		{t3.AutoAcceptEdits, "auto", "acceptEdits"},
		{t3.AutoAcceptEdits, "acceptEdits", "acceptEdits"},
		{t3.AutoAcceptEdits, "default", "default"},
		{t3.Auto, "bypassPermissions", "auto"},
		{t3.Auto, "auto", "auto"},
		{t3.FullAccess, "bypassPermissions", "bypassPermissions"},
		{t3.FullAccess, "default", "default"},
		{t3.ApprovalRequired, "plan", "plan"}, // plan only reads: it passes at any ceiling
		{t3.FullAccess, "plan", "plan"},
		{t3.FullAccess, "somethingNew", "default"}, // unknown: the most restricted
		{t3.Mode("nonsense"), "bypassPermissions", "default"},
	}
	for _, c := range cases {
		for _, eq := range []bool{false, true} {
			var in []string
			if eq {
				in = []string{"--permission-mode=" + c.want}
			} else {
				in = []string{"--permission-mode", c.want}
			}
			got, err := clamp(in, pol(c.ceiling))
			if err != nil {
				t.Fatal(err)
			}
			var w []string
			if eq {
				w = []string{"--permission-mode=" + c.out}
			} else {
				w = []string{"--permission-mode", c.out}
			}
			if !reflect.DeepEqual(got, w) {
				t.Errorf("ceiling %s, asked %s (eq=%v): got %q, want %q", c.ceiling, c.want, eq, got, w)
			}
		}
	}
}

func TestClampArgvEveryModeFlagIsClamped(t *testing.T) {
	got, err := clamp([]string{"--permission-mode", "bypassPermissions", "--permission-mode=bypassPermissions"}, pol(t3.ApprovalRequired))
	if err != nil || !reflect.DeepEqual(got, []string{"--permission-mode", "default", "--permission-mode=default"}) {
		t.Fatalf("%q %v", got, err)
	}
}

func TestClampArgvAddsTheLowestModeWhenNoneIsGiven(t *testing.T) {
	got, err := clampRaw([]string{"--verbose"}, pol(t3.FullAccess))
	if err != nil || !reflect.DeepEqual(got, []string{"--verbose", "--permission-mode", "default", "--strict-mcp-config"}) {
		t.Fatalf("%q %v", got, err)
	}
}

func TestClampArgvDropsTheSkipPermissionFlagsBelowFullAccess(t *testing.T) {
	in := []string{"--dangerously-skip-permissions", "--verbose", "--allow-dangerously-skip-permissions", "--permission-mode", "default"}
	for _, c := range []t3.Mode{t3.ApprovalRequired, t3.AutoAcceptEdits, t3.Auto, t3.Mode("")} {
		got, err := clamp(in, pol(c))
		if err != nil || !reflect.DeepEqual(got, []string{"--verbose", "--permission-mode", "default"}) {
			t.Errorf("%s: %q %v", c, got, err)
		}
	}
	got, err := clamp(in, pol(t3.FullAccess))
	if err != nil || !reflect.DeepEqual(got, in) {
		t.Errorf("full access keeps them: %q %v", got, err)
	}
	if _, err := clamp([]string{"--dangerously-skip-permissions=1"}, pol(t3.FullAccess)); err == nil {
		t.Error("a value on a flag without one")
	}
}

func TestClampArgvRefusesWhatCouldTurnTheHookOff(t *testing.T) {
	for _, bad := range [][]string{
		{"--settings", `{"disableAllHooks":true}`}, {"--settings", `{"hooks":{}}`}, {"--settings", `{"showThinkingSummaries":"yes"}`},
		{"--settings", `{"showThinkingSummaries":true,"env":{"X":"1"}}`}, {"--settings=/tmp/s.json"}, {"--settings", "/tmp/s.json"}, {"--settings", `[true]`}, {"--setting-sources", "user"}, {"--setting-sources=project"},
		{"--mcp-config", "/tmp/mcp.json"}, {"--mcp-config=./mcp.json"}, {"--mcp-config", `[1]`}, {"--mcp-config", `{bad`},
		{"--plugin-dir", "/tmp/p"}, {"--agents", "{}"}, {"--bare"},
		{"--add-dir", "/"}, {"--add-dir", "/Users"}, {"--add-dir=/etc"}, {"--add-dir", tCwd + "/../.."},
		{"--permission-prompt-tool", "mcp__evil__approve"}, {"--output-format", "text"}, {"--input-format=text"},
		{"--session-id", "../../x"}, {"--resume", "a b"}, {"--model"}, {"--model", "--settings"},
		{"-p", "hi"}, {"hello"}, {"--"}, {"--print"}, {"--some-new-flag"}, {"--verbose=1"},
	} {
		_, err := clamp(append(t3Argv("default"), bad...), pol(t3.FullAccess))
		var re *RefusedError
		if !errors.As(err, &re) {
			t.Errorf("%q: not refused (%v)", bad, err)
		} else if !strings.Contains(re.Error(), "Rubi won't start claude with") {
			t.Errorf("%q: message %q", bad, re.Error())
		}
	}
}

func TestClampArgvAllowsAddDirOfTheWorkspaceAndAttachmentsOnly(t *testing.T) {
	if _, err := clamp([]string{"--add-dir", tAtt}, Policy{Ceiling: t3.Auto, Cwd: tCwd}); err == nil {
		t.Error("attachments accepted when the policy names none")
	}
	got, err := clamp([]string{"--add-dir", tCwd + "/", "--add-dir=" + tAtt + "/."}, pol(t3.Auto))
	if err != nil || got[1] != tCwd || got[2] != "--add-dir="+tAtt {
		t.Errorf("cleaned: %q %v", got, err)
	}
}

func TestExtraDirsAreTheAddDirsBesidesTheWorkspace(t *testing.T) {
	c, err := ClampArgv(t3Argv("default", "--add-dir="+tAtt), pol(t3.Auto))
	if err != nil {
		t.Fatal(err)
	}
	if got := ExtraDirs(c, tCwd); !reflect.DeepEqual(got, []string{tAtt}) {
		t.Fatalf("got %q", got)
	}
	c, _ = ClampArgv([]string{"--add-dir", tCwd}, pol(t3.Auto))
	if ExtraDirs(c, tCwd) != nil {
		t.Error("the workspace listed as extra")
	}
}

func TestPermissionModeNames(t *testing.T) {
	for in, want := range map[t3.Mode]string{t3.ApprovalRequired: "default", t3.AutoAcceptEdits: "acceptEdits", t3.Auto: "auto", t3.FullAccess: "bypassPermissions", "": "default"} {
		if got := PermissionMode(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}

func TestClampArgvEmptyAndRepeatedPermissionModes(t *testing.T) {
	for _, in := range [][]string{{"--permission-mode="}, {"--permission-mode", ""}} {
		got, err := clamp(in, pol(t3.FullAccess))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got[0] != "--permission-mode=default" && !(got[0] == "--permission-mode" && got[1] == "default") {
			t.Errorf("%q became %q", in, got)
		}
	}
	got, err := clampRaw([]string{"--permission-mode", "plan", "--permission-mode=bypassPermissions", "--permission-mode", "acceptEdits"}, pol(t3.Auto))
	want := []string{"--permission-mode", "plan", "--permission-mode=auto", "--permission-mode", "acceptEdits", "--strict-mcp-config"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%q %v", got, err)
	}
}

// --add-dir is compared as a path, not resolved: a link that points out is not the workspace and is refused.
func TestClampArgvRefusesAnAddDirThatIsASymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	for _, in := range [][]string{{"--add-dir", link}, {"--add-dir=" + link}, {"--add-dir", link + "/../" + filepath.Base(dir)}} {
		if _, err := clamp(in, Policy{Ceiling: t3.Auto, Cwd: dir, Attachments: filepath.Join(dir, "att"), T3Port: tPort}); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestClampArgvOnlyT3sOwnToolsMayBePreApproved(t *testing.T) {
	for _, v := range []string{"Bash", "Bash(*)", "mcp__t3-code__*,Bash", "mcp__other__*", "*", "mcp__t3-code__"} {
		for _, in := range [][]string{{"--allowedTools", v}, {"--allowedTools=" + v}} {
			if _, err := clamp(in, pol(t3.FullAccess)); err == nil {
				t.Errorf("%q accepted", in)
			}
		}
	}
	if _, err := clamp([]string{"--allowedTools", "mcp__t3-code__*"}, pol(t3.FullAccess)); err != nil {
		t.Error(err)
	}
}

func TestClampArgvOnlyT3sOwnMCPServerIsAccepted(t *testing.T) {
	ok := func(servers string) string { return `{"mcpServers":` + servers + `}` }
	for name, cfg := range map[string]string{
		"stdio server":         ok(`{"x":{"type":"stdio","command":"sh","args":["-c","id"]}}`),
		"no type (stdio)":      ok(`{"x":{"command":"sh"}}`),
		"command beside http":  ok(`{"x":{"type":"http","url":"http://127.0.0.1:4410/mcp","command":"sh"}}`),
		"env beside http":      ok(`{"x":{"type":"http","url":"http://127.0.0.1:4410/mcp","env":{"A":"b"}}}`),
		"other port":           ok(`{"x":{"type":"http","url":"http://127.0.0.1:4411/mcp"}}`),
		"other host":           ok(`{"x":{"type":"http","url":"http://evil.example:4410/mcp"}}`),
		"localhost":            ok(`{"x":{"type":"http","url":"http://localhost:4410/mcp"}}`),
		"https":                ok(`{"x":{"type":"http","url":"https://127.0.0.1:4410/mcp"}}`),
		"userinfo":             ok(`{"x":{"type":"http","url":"http://127.0.0.1:4410@evil.example/mcp"}}`),
		"no port":              ok(`{"x":{"type":"http","url":"http://127.0.0.1/mcp"}}`),
		"ws type":              ok(`{"x":{"type":"ws","url":"http://127.0.0.1:4410/mcp"}}`),
		"second server is bad": ok(`{"t3":{"type":"http","url":"http://127.0.0.1:4410/mcp"},"x":{"type":"sse","url":"http://10.0.0.1:4410/"}}`),
		"extra top-level key":  `{"mcpServers":{},"settings":{"disableAllHooks":true}}`,
		"servers not a map":    ok(`[1]`),
		"file":                 "/tmp/mcp.json",
	} {
		if _, err := clamp([]string{"--mcp-config", cfg}, pol(t3.FullAccess)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := clamp([]string{"--mcp-config", t3MCP}, Policy{Ceiling: t3.Auto, Cwd: tCwd}); err == nil {
		t.Error("an MCP server accepted for a session without a T3 port")
	}
	if _, err := clamp([]string{"--mcp-config", ok(`{}`)}, pol(t3.Auto)); err != nil {
		t.Errorf("an empty server list: %v", err)
	}
}

// What T3 really passes (checked against the pinned nightly): --effort, the skip-permissions allowance, a repeated
// --thinking-display and the one setting it gives on the command line.
func TestClampArgvAcceptsT3sRealCommandLine(t *testing.T) {
	in := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json", "--thinking", "adaptive", "--thinking-display", "summarized",
		"--effort", "high", "--model", "claude-sonnet-4-6", "--permission-prompt-tool", "stdio", "--allowedTools", "mcp__t3-code__*", "--tools", "default",
		"--permission-mode", "bypassPermissions", "--allow-dangerously-skip-permissions", "--include-partial-messages", "--add-dir", tCwd, "--add-dir", tAtt,
		"--session-id=2f527ae3-2ff7-4174-8f30-9e1dbe399c39", "--thinking-display", "summarized", "--settings", `{"showThinkingSummaries":true}`}
	got, err := clamp(in, pol(t3.FullAccess))
	if err != nil || !reflect.DeepEqual(got, in) {
		t.Fatalf("%q %v", got, err)
	}
	got, err = clamp(in, pol(t3.ApprovalRequired))
	if err != nil {
		t.Fatal(err)
	}
	if contains(got, "--allow-dangerously-skip-permissions") || got[indexOf(got, "--permission-mode")+1] != "default" {
		t.Errorf("not clamped: %q", got)
	}
}

func indexOf(a []string, s string) int {
	for i, x := range a {
		if x == s {
			return i
		}
	}
	return -1
}
