package home

// The T3 threads Rubi Home started: which role and project each belongs to, who started it, what mode it runs in and
// whether a turn is running. T3 itself knows many more threads (the user's own); a Bot only ever sees these. The list
// is a private file (0600) so a restart of Home still knows its threads and resumes the watch on running turns.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

const maxT3Threads = 2000

type t3Thread struct {
	ID          string `json:"id"`
	Task        string `json:"task,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Environment string `json:"environment,omitempty"`
	Project     string `json:"project"`
	Role        string `json:"role"`
	Name        string `json:"name,omitempty"`
	// Bot is the Bot that started the thread ("" = the user's own).
	Bot string `json:"bot,omitempty"`
	// Mode is the claude-agent spelling of the permission mode the thread was started in (after clamping).
	Mode                 string    `json:"mode"`
	Runtime              string    `json:"runtime"`
	Created              time.Time `json:"created"`
	LastActivity         time.Time `json:"last_activity"`
	Closed               bool      `json:"closed,omitempty"`
	Running              bool      `json:"running,omitempty"`
	RunID                string    `json:"run_id,omitempty"`
	NativeCleanupPending bool      `json:"native_cleanup_pending,omitempty"`
	TaskReport           string    `json:"task_report,omitempty"`
	TaskOutcome          string    `json:"task_outcome,omitempty"`
	To                   string    `json:"to,omitempty"`       // who the next finished turn answers: the sender of the last message
	ReplyTo              []string  `json:"reply_to,omitempty"` // ids of the messages since the last finished turn
	LastPos              int       `json:"last_pos"`           // the last item of the thread already seen
}

type t3Threads struct {
	path   string
	mu     sync.Mutex
	loaded bool
	list   []t3Thread
}

type t3ThreadsDisk struct {
	Version int        `json:"version"`
	Threads []t3Thread `json:"threads"`
}

func (s *t3Threads) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var d t3ThreadsDisk
	if json.Unmarshal(b, &d) != nil || d.Version != 1 {
		_ = os.Rename(s.path, s.path+".bad")
		return
	}
	s.list = d.Threads
}

func (s *t3Threads) saveLocked() error {
	b, err := json.Marshal(t3ThreadsDisk{Version: 1, Threads: s.list})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return vault.WriteFileAtomic(s.path, b, 0o600)
}

func (s *t3Threads) get(id string) (t3Thread, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	for _, t := range s.list {
		if t.ID == id {
			return t, true
		}
	}
	return t3Thread{}, false
}

// add records a new thread; at the cap the oldest closed one makes room.
func (s *t3Threads) add(t t3Thread) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if len(s.list) >= maxT3Threads {
		drop := -1
		for i, x := range s.list {
			if x.Closed && !x.Running && (drop < 0 || x.LastActivity.Before(s.list[drop].LastActivity)) {
				drop = i
			}
		}
		if drop < 0 {
			return errors.New("too many sessions are open; close some first")
		}
		s.list = append(s.list[:drop], s.list[drop+1:]...)
	}
	s.list = append(s.list, t)
	return s.saveLocked()
}

// update changes one thread in place and saves; it reports whether the thread exists.
func (s *t3Threads) update(id string, fn func(*t3Thread)) (t3Thread, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	for i := range s.list {
		if s.list[i].ID == id {
			fn(&s.list[i])
			if err := s.saveLocked(); err != nil {
				_ = err // the list in memory is right; the file is retried at the next change
			}
			return s.list[i], true
		}
	}
	return t3Thread{}, false
}

// snapshot is a copy of the list.
func (s *t3Threads) snapshot() []t3Thread {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return append([]t3Thread(nil), s.list...)
}

// open lists the threads that are not closed, optionally of one project and role, oldest first.
func (s *t3Threads) open(project, role string) []t3Thread {
	var out []t3Thread
	for _, t := range s.snapshot() {
		if t.Closed || (project != "" && t.Project != project) || (role != "" && t.Role != role) {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// runningCounts is how many turns run now: by role, by project, by Bot and in all.
type t3Counts struct{ role, project, bot, total int }

func (s *t3Threads) counts(project, role, bot string) t3Counts {
	var c t3Counts
	for _, t := range s.snapshot() {
		if !t.Running || t.Closed {
			continue
		}
		c.total++
		if t.Project == project {
			c.project++
		}
		if t.Role == role {
			c.role++
		}
		if t.Bot == bot {
			c.bot++
		}
	}
	return c
}

// T3 thread IDs are opaque RPC identifiers, never filesystem names. Native MCP
// launches use mcp:launch:<digest>, unlike the legacy runner's filename IDs.
func validT3ThreadID(id string) bool {
	if id == "" || len(id) > 512 || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
