package home

import (
	"log"
	"strings"
	"time"
)

// Every hook Rubi Home handles leaves one line in its log:
//
//	claude-hook <event> session=<first 8 of the id> outcome=<what happened> <ms>ms
//
// The line has counts, ids and digests only, never a command, a message or a reply text. It is written by
// log.Printf with no lock held, after the work, so logging never holds a hook up.

// hookToken makes a hook-supplied word safe for a log line: at most max characters of letters, digits,
// and . _ - : (anything else becomes "?").
func hookToken(s string, max int) string {
	if s == "" {
		return "-"
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == max {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-', r == ':':
			b.WriteRune(r)
		default:
			b.WriteRune('?')
		}
		n++
	}
	return b.String()
}

// logHook writes the hook line. session is the full id (shortened here).
func logHook(event, session, outcome string, start time.Time) {
	log.Printf("claude-hook %s session=%s outcome=%s %dms", hookToken(event, 32), hookToken(session, 8), outcome, time.Since(start).Milliseconds())
}
