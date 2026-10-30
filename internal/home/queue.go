package home

// The queue file. Messages for sessions, what sessions said (replies, asks, message statuses) and the
// replies a turn still owes are kept in <helper dir>/claude-queue.json (0600), so a restart of the helper
// loses nothing. The saver writes a snapshot at most claudeSaveDelay after a change (and when the helper
// stops); it takes the snapshot under c.mu and writes outside it, so a slow disk can only hold up the saver,
// never a hook. A message still waiting after claudeQueueTTL expires (its sender gets an "expired" status);
// an outbox entry that old is dropped. Chain counters are not kept.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// messageIDRE is the shape of the helper's message ids (cm_ and 12 characters of rand.Text).
var messageIDRE = regexp.MustCompile(`^cm_[A-Za-z0-9]{1,40}$`)

// claudeMaxQueueFile is the largest queue file that is read (a bigger one is moved aside).
const claudeMaxQueueFile = 8 << 20

// ClaudeQueueFile is the queue file's name in the helper's directory.
const ClaudeQueueFile = "claude-queue.json"

var (
	claudeQueueTTL  = 24 * time.Hour
	claudeSaveDelay = 500 * time.Millisecond
	claudeSweepEach = time.Minute
)

type queueOwed struct {
	From   []string `json:"from"`
	IDs    []string `json:"ids"`
	IDFrom []string `json:"id_from,omitempty"`
}

type queueFile struct {
	Version int                        `json:"version"`
	SavedAt time.Time                  `json:"saved_at"`
	Inbox   map[string][]ClaudeMessage `json:"inbox,omitempty"`
	Outbox  []ClaudeOut                `json:"outbox,omitempty"`
	Owed    map[string]queueOwed       `json:"owed,omitempty"`
	// Bound and Delivered were added after version 1 shipped: optional keys, so the version stays 1 (an older
	// helper ignores them).
	Bound     map[string][]boundSister  `json:"bound,omitempty"`
	Delivered map[string][]deliveredMsg `json:"delivered,omitempty"`
}

// dirtyLocked tells the saver that the queue changed. Callers hold c.mu.
func (c *Claude) dirtyLocked() {
	if c.QueueFile == "" {
		return
	}
	if c.saveKick == nil {
		c.saveKick = make(chan struct{}, 1)
	}
	select {
	case c.saveKick <- struct{}{}:
	default:
	}
}

// sweepLocked expires inbox messages and drops outbox entries older than claudeQueueTTL.
func (c *Claude) sweepLocked(now time.Time) {
	for id, msgs := range c.inbox {
		keep := msgs[:0:0]
		for _, m := range msgs {
			if now.Sub(m.At) > claudeQueueTTL {
				c.statusLocked(id, "expired", m)
				continue
			}
			keep = append(keep, m)
		}
		if len(keep) == len(msgs) {
			continue
		}
		c.dirtyLocked()
		if len(keep) == 0 {
			delete(c.inbox, id)
		} else {
			c.inbox[id] = keep
		}
	}
	for id := range c.delivered {
		if before := len(c.delivered[id]); len(c.pruneDeliveredLocked(id, now)) != before {
			c.dirtyLocked()
		}
	}
	keep := c.outbox[:0:0]
	for _, o := range c.outbox {
		if now.Sub(o.At) <= claudeQueueTTL {
			keep = append(keep, o)
		}
	}
	if len(keep) != len(c.outbox) {
		c.outbox = keep
		c.dirtyLocked()
	}
}

// snapshotLocked copies what is to be saved; in-flight wake messages count as waiting.
func (c *Claude) snapshotLocked() queueFile {
	q := queueFile{Version: 1, SavedAt: time.Now().UTC(), Inbox: map[string][]ClaudeMessage{}, Owed: map[string]queueOwed{}}
	for id, m := range c.inflight {
		q.Inbox[id] = append(q.Inbox[id], m...)
	}
	for id, m := range c.inbox {
		q.Inbox[id] = append(q.Inbox[id], m...)
	}
	q.Outbox = append([]ClaudeOut(nil), c.outbox...)
	if len(c.bound) > 0 {
		q.Bound = map[string][]boundSister{}
		for id, b := range c.bound {
			q.Bound[id] = append([]boundSister(nil), b...)
		}
	}
	if len(c.delivered) > 0 {
		q.Delivered = map[string][]deliveredMsg{}
		for id, d := range c.delivered {
			q.Delivered[id] = append([]deliveredMsg(nil), d...)
		}
	}
	for id, t := range c.turns {
		if len(t.from) > 0 {
			q.Owed[id] = queueOwed{From: append([]string(nil), t.from...), IDs: append([]string(nil), t.ids...),
				IDFrom: append([]string(nil), t.idFrom...)}
		}
	}
	return q
}

// saveQueue writes the queue file now (no-op without QueueFile).
func (c *Claude) saveQueue() error {
	if c.QueueFile == "" {
		return nil
	}
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.mu.Lock()
	c.sweepLocked(time.Now().UTC())
	q := c.snapshotLocked()
	write := c.writeQueue
	c.mu.Unlock()
	b, err := json.Marshal(q)
	if err != nil {
		return err
	}
	if write == nil {
		write = writeFileAtomic
	}
	return write(c.QueueFile, b)
}

// writeFileAtomic writes b to path through a temp file in the same directory (0600) and a rename.
func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".claude-queue-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// runSaver saves the queue after changes until ctx ends, then once more.
func (c *Claude) runSaver(ctx context.Context) {
	c.mu.Lock()
	if c.saveKick == nil {
		c.saveKick = make(chan struct{}, 1)
	}
	kick := c.saveKick
	c.mu.Unlock()
	tick := time.NewTicker(claudeSweepEach)
	defer tick.Stop()
	save := func() {
		if err := c.saveQueue(); err != nil {
			log.Printf("claude: saving the queue: %v", err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			save()
			return
		case <-tick.C:
			c.mu.Lock()
			c.sweepLocked(time.Now().UTC())
			c.mu.Unlock()
		case <-kick:
			select { // let a burst of changes become one write
			case <-time.After(claudeSaveDelay):
			case <-ctx.Done():
			}
			save()
		}
	}
}

// loadQueue reads the queue file (no-op without QueueFile). A file that can't be read as a queue is
// moved aside to <file>.bad and the queue starts empty. Expired entries are dealt with as usual.
func (c *Claude) loadQueue() {
	if c.QueueFile == "" {
		return
	}
	var b []byte
	f, err := os.Open(c.QueueFile)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err == nil {
		b, err = io.ReadAll(io.LimitReader(f, claudeMaxQueueFile+1))
		f.Close()
		if err == nil && len(b) > claudeMaxQueueFile {
			err = errors.New("file too large")
		}
	}
	var q queueFile
	if err == nil {
		err = json.Unmarshal(b, &q)
	}
	if err == nil && q.Version != 1 {
		err = errors.New("unknown version")
	}
	if err != nil {
		log.Printf("claude: the queue file %s is unusable (%v); moved aside, starting empty", c.QueueFile, err)
		_ = os.Rename(c.QueueFile, c.QueueFile+".bad")
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, msgs := range q.Inbox {
		if id == "" || len(id) > 200 || len(msgs) == 0 {
			continue
		}
		if c.inbox == nil {
			c.inbox = map[string][]ClaudeMessage{}
		}
		for _, m := range msgs {
			if len(c.inbox[id]) >= claudeMaxInbox {
				break
			}
			m.From = oneLineText(m.From, claudeMaxFrom)
			m.Text, _ = clip(m.Text, claudeMaxText)
			c.inbox[id] = append(c.inbox[id], m)
		}
	}
	for _, o := range q.Outbox {
		if o.Kind != "reply" && o.Kind != "ask" && o.Kind != "status" {
			continue
		}
		if isLocalSender(o.To) { // never for a Bot, whatever a saved file says
			continue
		}
		if o.Kind != "status" {
			o.Text, _ = clip(o.Text, claudeMaxReply)
		}
		c.outbox = append(c.outbox, o)
	}
	if len(c.outbox) > claudeMaxOut { // the newest stay
		c.outbox = append([]ClaudeOut(nil), c.outbox[len(c.outbox)-claudeMaxOut:]...)
	}
	for id, ow := range q.Owed {
		if id == "" || len(ow.From) == 0 {
			continue
		}
		t := c.turnLocked(id)
		t.from, t.ids, t.idFrom = ow.From, ow.IDs, ow.IDFrom
	}
	// Bound sisters: a bounded number per session and in all, and a Bot has one binding (the newest wins).
	type bnd struct {
		id string
		b  boundSister
	}
	newest := map[string]bnd{}
	for id, bs := range q.Bound {
		if id == "" || len(id) > 200 {
			continue
		}
		for _, b := range bs {
			b.Sister = oneLineText(b.Sister, claudeMaxFrom)
			if b.Sister == "" {
				continue
			}
			k := strings.ToLower(b.Sister)
			if cur, ok := newest[k]; !ok || b.Since.After(cur.b.Since) {
				newest[k] = bnd{id, b}
			}
		}
	}
	order := make([]bnd, 0, len(newest))
	for _, x := range newest {
		order = append(order, x)
	}
	sort.Slice(order, func(i, j int) bool { // newest first, so a full list drops the oldest
		if !order[i].b.Since.Equal(order[j].b.Since) {
			return order[i].b.Since.After(order[j].b.Since)
		}
		return order[i].b.Sister < order[j].b.Sister
	})
	for _, x := range order {
		if len(c.bound) >= claudeMaxBound && len(c.bound[x.id]) == 0 {
			continue
		}
		if len(c.bound[x.id]) >= claudeMaxBoundPerSession {
			continue
		}
		if x.b.Since.After(time.Now()) {
			x.b.Since = time.Now().UTC()
		}
		if c.bound == nil {
			c.bound = map[string][]boundSister{}
		}
		c.bound[x.id] = append(c.bound[x.id], x.b)
	}
	// Delivered messages: ids as the helper makes them, short one-line senders, no times in the future.
	nowU := time.Now().UTC()
	for id, ds := range q.Delivered {
		if id == "" || len(id) > 200 || (len(c.delivered) >= claudeMaxBound && len(c.delivered[id]) == 0) {
			continue
		}
		if len(ds) > claudeMaxDelivered {
			ds = ds[len(ds)-claudeMaxDelivered:]
		}
		for _, d := range ds {
			if !messageIDRE.MatchString(d.ID) {
				continue
			}
			d.From = oneLineText(d.From, claudeMaxFrom)
			if d.At.After(nowU) {
				d.At = nowU
			}
			if c.delivered == nil {
				c.delivered = map[string][]deliveredMsg{}
			}
			c.delivered[id] = append(c.delivered[id], d)
		}
	}
	c.sweepLocked(time.Now().UTC())
}
