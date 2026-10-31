package sshtarget

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode"
)

const (
	auditFile    = "audit.jsonl"
	auditMaxSize = 1 << 20 // the file moves to audit.jsonl.1 past this
	// CommandClip is how much of a command the audit keeps.
	CommandClip = 200
)

// Entry is one audit line: one stream (or one refusal). It has no output and no key material.
type Entry struct {
	Stream   string `json:"stream,omitempty"`
	Role     string `json:"role"`
	Project  string `json:"project"`
	Bot      string `json:"bot,omitempty"`
	Target   string `json:"target"`
	Command  string `json:"command"`
	Start    string `json:"start"`
	End      string `json:"end"`
	Exit     *int   `json:"exit,omitempty"`
	BytesIn  int64  `json:"bytes_in"`  // runner to target
	BytesOut int64  `json:"bytes_out"` // target to runner
	// Result is "ok" for a stream that ran, else why there was none or how it was cut: "refused", "host key
	// changed", "denied", "revoked", "idle timeout", "time limit", "error".
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
}

var secretArg = regexp.MustCompile(`(?i)((?:pass(?:word|wd)?|token|secret|api[_-]?key|authorization|bearer|credential)s?["']?\s*[=:]\s*(?:(?:bearer|basic|token)\s+)?|bearer\s+|--(?:password|token|secret)\s+)\S+`)

// redact hides what looks like a secret in a command line.
func redact(s string) string {
	return secretArg.ReplaceAllString(s, "${1}[redacted]")
}

// ClipCommand redacts and clips a command for the audit and the approval item. Control characters become spaces.
func ClipCommand(s string, n int, extra func(string) string) string {
	s = redact(s)
	if extra != nil {
		s = extra(s)
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "..."
	}
	return s
}

// Audit is the append-only audit file in the registry directory.
type Audit struct {
	path string
	mu   sync.Mutex
}

// NewAudit audits into dir/audit.jsonl.
func NewAudit(dir string) *Audit { return &Audit{path: filepath.Join(dir, auditFile)} }

// Add appends an entry.
func (a *Audit) Add(e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if st, err := os.Stat(a.path); err == nil && st.Size() > auditMaxSize {
		_ = os.Rename(a.path, a.path+".1")
	}
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Recent returns up to limit entries, newest first, of one target when target is set.
func (a *Audit) Recent(target string, limit int) []Entry {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var all []Entry
	for _, p := range []string{a.path + ".1", a.path} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var e Entry
			if json.Unmarshal(sc.Bytes(), &e) == nil && (target == "" || e.Target == target) {
				all = append(all, e)
			}
		}
	}
	out := []Entry{}
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, all[i])
	}
	return out
}
