package sshtarget

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	auditFile    = "audit.jsonl"         // streams, and refusals that matter (denied, host key changed)
	noiseFile    = "audit-refused.jsonl" // plain refusals and errors: a flood of them can't push the others out
	auditMaxSize = 1 << 20               // a file moves to <file>.1 past this
	noiseMaxSize = 256 << 10
	dupWindow    = 5 * time.Minute // identical refusals within this collapse into a counter
	// CommandClip is how much of a command the audit keeps.
	CommandClip = 200
)

// Entry is one audit line: one stream (or one refusal). It has no output and no key material.
type Entry struct {
	// Seq numbers the entries in the order they were written (1, 2, ...), across restarts and rotations, so a
	// reader can copy the audit without loss: ask for the entries after the last Seq it has.
	Seq      uint64 `json:"seq,omitempty"`
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
	// Count, when set, says this entry stands for that many further identical refusals that were not written one
	// by one (End is the time of the last).
	Count int `json:"count,omitempty"`
}

func (e Entry) noise() bool { return e.Result == "refused" || e.Result == "error" }

func (e Entry) key() string {
	return e.Role + "\x00" + e.Target + "\x00" + e.Result + "\x00" + e.Reason + "\x00" + e.Command
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

// Audit is the append-only audit in the registry directory: audit.jsonl and audit-refused.jsonl, each with one
// rotated predecessor (.1).
type Audit struct {
	dir string
	now func() time.Time
	mu  sync.Mutex

	seq     uint64
	loaded  bool
	last    Entry // the last refusal written, to collapse repeats of it
	lastKey string
	lastAt  time.Time
	dups    int
	dupEnd  string
}

// NewAudit audits into dir.
func NewAudit(dir string) *Audit { return &Audit{dir: dir, now: time.Now} }

func (a *Audit) path(noise bool) string {
	if noise {
		return filepath.Join(a.dir, noiseFile)
	}
	return filepath.Join(a.dir, auditFile)
}

func readEntries(p string) []Entry {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func (a *Audit) loadLocked() {
	if a.loaded {
		return
	}
	a.loaded = true
	for _, n := range []bool{false, true} {
		for _, p := range []string{a.path(n) + ".1", a.path(n)} {
			for _, e := range readEntries(p) {
				a.seq = max(a.seq, e.Seq)
			}
		}
	}
}

func (a *Audit) writeLocked(e Entry) error {
	a.loadLocked()
	a.seq++
	e.Seq = a.seq
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	p, limit := a.path(e.noise()), int64(auditMaxSize)
	if e.noise() {
		limit = noiseMaxSize
	}
	if st, err := os.Stat(p); err == nil && st.Size() > limit {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// flushLocked writes the counter of collapsed repeats, if any.
func (a *Audit) flushLocked() error {
	if a.dups == 0 {
		return nil
	}
	e := a.last
	e.Count, e.End = a.dups, a.dupEnd
	a.dups = 0
	return a.writeLocked(e)
}

// Add appends an entry. A refusal identical to the previous one (within a few minutes) is not written but
// counted; the count is written as one entry (Count) before the next different entry or when the audit is read.
func (a *Audit) Add(e Entry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if e.noise() {
		k := e.key()
		if a.lastKey == k && now.Sub(a.lastAt) < dupWindow {
			a.dups++
			a.dupEnd = e.End
			return nil
		}
		err := a.flushLocked()
		a.last, a.lastKey, a.lastAt = e, k, now
		if werr := a.writeLocked(e); err == nil {
			err = werr
		}
		return err
	}
	err := a.flushLocked()
	a.lastKey = ""
	if werr := a.writeLocked(e); err == nil {
		err = werr
	}
	return err
}

func (a *Audit) allLocked(target string) []Entry {
	_ = a.flushLocked()
	var all []Entry
	for _, n := range []bool{false, true} {
		for _, p := range []string{a.path(n) + ".1", a.path(n)} {
			for _, e := range readEntries(p) {
				if target == "" || e.Target == target {
					all = append(all, e)
				}
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Seq < all[j].Seq })
	return all
}

// Recent returns up to limit entries, newest first, of one target when target is set.
func (a *Audit) Recent(target string, limit int) []Entry {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	all := a.allLocked(target)
	out := []Entry{}
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, all[i])
	}
	return out
}

// After returns up to limit entries with a Seq above after, oldest first (so a reader pages forward from the last
// Seq it got), and the highest Seq written so far.
func (a *Audit) After(target string, after uint64, limit int) ([]Entry, uint64) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	all := a.allLocked(target)
	a.loadLocked()
	out := []Entry{}
	for _, e := range all {
		if e.Seq > after && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, a.seq
}
