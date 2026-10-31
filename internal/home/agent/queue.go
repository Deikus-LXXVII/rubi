package agent

// The disk queue: starts and sends that found no free slot (their project's or the total limit) wait in
// <helper dir>/agent-queue.json (0600, written atomically) and are delivered in order when one frees. They survive a restart of Home. An
// item still waiting after QueueTTL expires (its sender is told through claude-agent.next). Delivery is
// at most once: an item leaves the file when it is handed to the runner, so a crash in between loses it
// rather than sending it twice.

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// QueueFile is the queue file's name in the helper's directory.
const QueueFile = "agent-queue.json"

// RolesDir is the directory of the role store in the helper's directory.
const RolesDir = "agent-roles"

const (
	maxQueueItems = 64
	maxQueueText  = 64 << 10
	maxQueueFile  = 8 << 20
)

// QueueTTL is how long an item may wait.
var QueueTTL = 24 * time.Hour

// QueueItem is a start or send waiting for a slot.
type QueueItem struct {
	ID      string    `json:"id"`
	Op      string    `json:"op"` // "start" or "send"
	Project string    `json:"project"`
	Role    string    `json:"role,omitempty"` // an isolated role's id; "" = the project's legacy role
	Bot     string    `json:"bot,omitempty"`  // the Bot that sent it ("" = the user)
	Session string    `json:"session,omitempty"`
	Text    string    `json:"text"`
	Name    string    `json:"name,omitempty"`
	From    string    `json:"from,omitempty"`
	Mode    string    `json:"mode,omitempty"`
	Steer   bool      `json:"steer,omitempty"`
	At      time.Time `json:"at"`
}

// rk is the key of the item's role: its id, or the project id for the legacy role.
func (it QueueItem) rk() string {
	if it.Role != "" {
		return it.Role
	}
	return it.Project
}

type queueDisk struct {
	Version int         `json:"version"`
	SavedAt time.Time   `json:"saved_at"`
	Items   []QueueItem `json:"items"`
}

var (
	errQueueFull = errors.New("too many messages are already waiting for the agent; try later")
	errTooLong   = errors.New("the text is too long to queue")
)

type diskQueue struct {
	path  string
	write func(path string, b []byte) error // a seam for tests

	mu    sync.Mutex
	items []QueueItem
}

func newQueueID() string { return "aq_" + rand.Text() }

// load reads the file; an unusable one is moved aside.
func (q *diskQueue) load() {
	if q.path == "" {
		return
	}
	f, err := os.Open(q.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var d queueDisk
	if err == nil {
		var b []byte
		b, err = io.ReadAll(io.LimitReader(f, maxQueueFile+1))
		f.Close()
		if err == nil && len(b) > maxQueueFile {
			err = errors.New("file too large")
		}
		if err == nil {
			err = json.Unmarshal(b, &d)
		}
		if err == nil && d.Version != 1 {
			err = errors.New("unknown version")
		}
	}
	if err != nil {
		log.Printf("agent: the queue file %s is unusable (%v); moved aside, starting empty", q.path, err)
		_ = os.Rename(q.path, q.path+".bad")
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, it := range d.Items {
		if len(q.items) < maxQueueItems && it.ID != "" && (it.Op == "start" || it.Op == "send") && len(it.Text) <= maxQueueText {
			q.items = append(q.items, it)
		}
	}
}

// saveLocked writes the file. Callers hold q.mu.
func (q *diskQueue) saveLocked() {
	if q.path == "" {
		return
	}
	b, err := json.Marshal(queueDisk{Version: 1, SavedAt: time.Now().UTC(), Items: q.items})
	if err == nil {
		w := q.write
		if w == nil {
			w = func(p string, b []byte) error { return vault.WriteFileAtomic(p, b, 0o600) }
		}
		err = w(q.path, b)
	}
	if err != nil {
		log.Printf("agent: saving the queue: %v", err)
	}
}

// add appends an item and returns its position (1 = next).
func (q *diskQueue) add(it QueueItem) (int, error) {
	if len(it.Text) > maxQueueText {
		return 0, errTooLong
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) >= maxQueueItems {
		return 0, errQueueFull
	}
	q.items = append(q.items, it)
	q.saveLocked()
	return len(q.items), nil
}

// take removes and returns the first item the test accepts.
func (q *diskQueue) take(ok func(QueueItem) bool) (QueueItem, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, it := range q.items {
		if ok(it) {
			q.items = append(q.items[:i:i], q.items[i+1:]...)
			q.saveLocked()
			return it, true
		}
	}
	return QueueItem{}, false
}

func (q *diskQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

func (q *diskQueue) list() []QueueItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]QueueItem(nil), q.items...)
}

// sweep removes items older than QueueTTL and returns them.
func (q *diskQueue) sweep(now time.Time) []QueueItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	var gone []QueueItem
	keep := q.items[:0:0]
	for _, it := range q.items {
		if now.Sub(it.At) > QueueTTL {
			gone = append(gone, it)
		} else {
			keep = append(keep, it)
		}
	}
	if len(gone) > 0 {
		q.items = keep
		q.saveLocked()
	}
	return gone
}
