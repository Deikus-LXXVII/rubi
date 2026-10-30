package home

// Versioning of the hook binary. Claude Code runs a root-owned copy of rubi-home
// (/usr/local/bin/rubi-home) that only changes when the user re-installs it, so it can fall behind the
// helper. Every line a hook sends carries rubi_hook_version, rubi_hook_protocol and rubi_hook_path; the
// helper reads them first (noteHook), refuses a protocol it doesn't speak, marks a legacy or older hook
// stale, and remembers the last hook seen for claude-sessions.list. Unknown fields are ignored by older
// helpers, and a legacy hook (no fields) is still served.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Deikus-LXXVII/rubi/internal/version"
)

const (
	// ClaudeHookProtocol is the version of the socket protocol this hook speaks. The helper serves the
	// protocols from claudeMinProtocol to claudeMaxProtocol.
	ClaudeHookProtocol = 1
	claudeMinProtocol  = 1
	claudeMaxProtocol  = 1
	// claudeStampRoom is how much longer than claudeMaxInput a socket line may be: the hook's fields.
	claudeStampRoom = 2048
	hookFieldMax    = 300
)

// InstallHookCommand is what the refusal and the plugin tell the user to run.
const InstallHookCommand = "~/.rubi-home/bin/rubi-home install-hook"

// hookSelf is this binary's version and path as a hook reports them (a variable for tests).
var hookSelf = func() (string, string) {
	p, _ := os.Executable()
	return version.Version, p
}

// helperVersion is the running helper's version (a variable for tests).
var helperVersion = func() string { return version.Version }

// stampHookLine adds the hook's version fields to one compact JSON object (a line without its newline).
// It splices the text in, so nothing already in the line (tool_input and its digest) changes. A later
// duplicate key wins in Go's decoder, so the fields can't be pre-empted by the input. Anything that isn't
// an object is returned as it is.
func stampHookLine(line []byte) []byte {
	line = bytes.TrimSpace(line)
	if len(line) < 2 || line[0] != '{' || line[len(line)-1] != '}' {
		return line
	}
	ver, path := hookSelf()
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	var f strings.Builder
	f.WriteString(`"rubi_hook_version":` + q(ver) + `,"rubi_hook_protocol":` + strconv.Itoa(ClaudeHookProtocol))
	if path != "" && len(path) <= hookFieldMax && !strings.ContainsFunc(path, unicode.IsControl) {
		f.WriteString(`,"rubi_hook_path":` + q(path))
	}
	body := bytes.TrimSpace(line[1 : len(line)-1])
	out := make([]byte, 0, len(line)+f.Len()+2)
	out = append(out, '{')
	if len(body) > 0 {
		out = append(out, body...)
		out = append(out, ',')
	}
	out = append(out, f.String()...)
	return append(out, '}')
}

// hookSeen is the last hook that spoke to the helper.
type hookSeen struct {
	Version  string
	Path     string
	Protocol int
	Legacy   bool // no version fields: a v0.12 hook
	Stale    bool
	Refused  bool
	At       time.Time
}

// hookState holds the last hook seen and which ones were logged. Its lock is taken alone: never together
// with Claude.mu or wmu, and nothing slow runs under it.
type hookState struct {
	mu     sync.Mutex
	last   *hookSeen
	logged map[string]bool
}

func (h *hookState) set(s *hookSeen) {
	h.mu.Lock()
	h.last = s
	h.mu.Unlock()
}

// cleanField keeps a hook-supplied string short and printable.
func cleanField(s string) string {
	s = cleanMessage(s)
	if r := []rune(s); len(r) > hookFieldMax {
		s = string(r[:hookFieldMax])
	}
	return s
}

// noteHook reads the version fields of a socket line, records the hook and returns a refusal text when the
// hook speaks a protocol the helper doesn't ("" otherwise). Lines without a hook_event_name aren't hooks'
// and are left to the caller.
func (c *Claude) noteHook(line []byte) string {
	var in struct {
		Event    json.RawMessage `json:"hook_event_name"`
		Version  json.RawMessage `json:"rubi_hook_version"`
		Path     json.RawMessage `json:"rubi_hook_path"`
		Protocol json.RawMessage `json:"rubi_hook_protocol"`
	}
	if err := json.Unmarshal(line, &in); err != nil {
		if bytes.Contains(line, []byte(`"hook_event_name"`)) { // a hook's line that can't be read: fail closed
			c.hooks.set(&hookSeen{Protocol: -1, Refused: true, At: time.Now().UTC()})
			return fmt.Sprintf("the Claude Code hook binary sent a line Rubi Home can't read: run `%s`; denied.", InstallHookCommand)
		}
		return ""
	}
	if len(in.Event) == 0 {
		return ""
	}
	var ver, path string
	typed := true // every stamp field that is present is a JSON string (the protocol a number)
	for _, f := range []struct {
		raw json.RawMessage
		dst *string
	}{{in.Version, &ver}, {in.Path, &path}} {
		if len(f.raw) > 0 && string(f.raw) != "null" && json.Unmarshal(f.raw, f.dst) != nil {
			typed = false
		}
	}
	seen := hookSeen{Version: cleanField(ver), Path: cleanField(path), At: time.Now().UTC()}
	var refuse, logMsg string
	home := helperVersion()
	where := seen.Path
	if where == "" {
		where = "unknown path"
	}
	p := strings.TrimSpace(string(in.Protocol))
	if !typed {
		p = `"unreadable"` // fields of the wrong type: refused like an unknown protocol
	}
	switch {
	case p == "" || p == "null":
		seen.Legacy, seen.Stale = true, true
		logMsg = "the Claude Code hook binary (" + where + ") is a legacy one without version information; it works, but run `" + InstallHookCommand + "` to update it"
	default:
		n, err := strconv.Atoi(p)
		shown := cleanField(p)
		if err != nil {
			n, shown = -1, "(unreadable)"
		}
		seen.Protocol = n
		if n < claudeMinProtocol || n > claudeMaxProtocol {
			seen.Refused, seen.Stale = true, versionOlder(seen.Version, home)
			refuse = fmt.Sprintf("the Claude Code hook binary speaks protocol %s, Rubi Home speaks %d: run `%s`; denied.",
				shown, claudeMaxProtocol, InstallHookCommand)
			logMsg = "the Claude Code hook binary (" + where + ") speaks protocol " + shown + ", Rubi Home speaks " + strconv.Itoa(claudeMaxProtocol) + "; refused"
		} else if versionOlder(seen.Version, home) {
			seen.Stale = true
			logMsg = "the Claude Code hook binary (" + where + ") is " + seen.Version + ", older than Rubi Home " + home + "; run `" + InstallHookCommand + "`"
		}
	}
	key := seen.Version + "|" + seen.Path + "|" + strconv.Itoa(seen.Protocol) + "|" + strconv.FormatBool(seen.Legacy)
	h := &c.hooks
	h.mu.Lock()
	h.last = &seen
	first := false
	if logMsg != "" && !h.logged[key] && len(h.logged) < 64 {
		if h.logged == nil {
			h.logged = map[string]bool{}
		}
		h.logged[key] = true
		first = true
	}
	h.mu.Unlock()
	if first {
		log.Printf("claude: %s", logMsg)
	}
	return refuse
}

// hookInfo is the last hook seen as claude-sessions.list reports it (nil when none was seen).
func (c *Claude) hookInfo() map[string]any {
	c.hooks.mu.Lock()
	s := c.hooks.last
	c.hooks.mu.Unlock()
	if s == nil {
		return nil
	}
	m := map[string]any{"version": shownVersion(s.Version), "path": shownPath(s.Path), "protocol": s.Protocol, "stale": s.Stale,
		"seen_at": s.At, "home_version": helperVersion()}
	if s.Legacy {
		m["legacy"] = true
	}
	if s.Refused {
		m["refused"] = true
	}
	return m
}

// shownVersion and shownPath are what Bots are shown of the hook's self-reported identity: advisory
// text, restricted to a version's characters and to printable characters.
func shownVersion(v string) string {
	v = strings.Map(func(r rune) rune {
		if r < 128 && (r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '.' || r == '+' || r == '-') {
			return r
		}
		return -1
	}, v)
	if len(v) > 40 {
		v = v[:40]
	}
	return v
}

func shownPath(p string) string {
	p = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, p)
	if r := []rune(p); len(r) > 200 {
		p = string(r[:200])
	}
	return p
}

// semver reads "v1.2.3", "1.2.3-rc1" or "v1.2.3-4-gabc" (git describe); ok is false for anything else
// (dev builds, empty).
func semver(s string) (nums [3]int, pre bool, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	s, _, _ = strings.Cut(s, "+")
	core, rest, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return nums, false, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return nums, false, false
		}
		nums[i] = n
	}
	return nums, hasPre && rest != "", true
}

// versionOlder reports whether version a is older than b. Versions that can't be compared (a dev build, an
// empty string) are never older.
func versionOlder(a, b string) bool {
	an, apre, aok := semver(a)
	bn, bpre, bok := semver(b)
	if !aok || !bok {
		return false
	}
	for i := range an {
		if an[i] != bn[i] {
			return an[i] < bn[i]
		}
	}
	return apre && !bpre
}
