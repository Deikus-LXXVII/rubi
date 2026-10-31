package setup

import (
	"strings"
	"unicode"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

const (
	maxProgress = 20  // lines kept of the running job
	maxLineLen  = 200 // characters per line
)

// clean makes a line fit for the progress list: control characters become spaces and it is cut to 200
// characters. ok is false for a line that looks like it holds a secret (see the design, "Secrets"): that
// line is dropped.
func clean(line string) (string, bool) {
	line = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, line))
	if line == "" {
		return "", false
	}
	if agent.LooksSecret(line) {
		return "", false
	}
	if r := []rune(line); len(r) > maxLineLen {
		line = string(r[:maxLineLen-1]) + "…"
	}
	return line, true
}

// safeText is clean for a multi-line text (an error message, command output) shown as a reason: the lines
// that pass, joined, at most 300 characters.
func safeText(s string) string {
	var keep []string
	for _, l := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if c, ok := clean(l); ok {
			keep = append(keep, c)
		}
	}
	out := strings.Join(keep, "; ")
	if r := []rune(out); len(r) > 300 {
		out = string(r[:299]) + "…"
	}
	return out
}

// ring keeps the last maxProgress lines.
type ring struct{ lines []string }

func (r *ring) add(line string) {
	c, ok := clean(line)
	if !ok {
		return
	}
	r.lines = append(r.lines, c)
	if len(r.lines) > maxProgress {
		r.lines = append([]string(nil), r.lines[len(r.lines)-maxProgress:]...)
	}
}

func (r *ring) snapshot() []string { return append([]string(nil), r.lines...) }

// tail is the last n lines as a reason.
func (r *ring) tail(n int) string {
	l := r.lines
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "; ")
}
