package core

import (
	"bufio"
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/audit"
	"github.com/Deikus-LXXVII/rubi/internal/e2e"
	"github.com/Deikus-LXXVII/rubi/internal/events"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// While Rubi is locked, nothing that needs the user's data can run: plugins are stopped and the vault is
// closed. Two things still must not be lost, and the Bots should hear about the gap afterwards:
//
//   - Events not yet reported to the user. They are kept in the vault (sealed) as they happen, so neither a
//     lock nor a restart drops them, and come back on unlock.
//   - Hook requests (a Shortcut saying "arrived home") that arrive while locked. They are sealed to the
//     lockbox key, whose private half is in the vault, and kept on disk; on unlock they are opened and
//     handed to their plugins. Nobody can read them in between, not even with the disk.
//
// After unlocking, every Bot with events waiting gets one notice: Rubi was locked from ... to ..., and
// how many events wait for it.

const (
	maxPendingEvents = 200
	maxWaitingHooks  = 500
	maxWaitingBytes  = 2 << 20
	lockboxPath      = "/lockbox/v1"
)

var backlogMu sync.Mutex // serializes writes of the waiting-hooks file

// saveEvents keeps the unreported events in the vault.
func (c *Core) saveEvents() {
	if c.State() != Unlocked {
		return
	}
	list := c.Events.List(false)
	if len(list) > maxPendingEvents {
		list = list[:maxPendingEvents] // newest first
	}
	_ = c.Vault.Update(func(d *vault.Data) error {
		d.PendingEvents = list
		return nil
	})
}

// beforeLock records when Rubi locked and keeps the unreported events.
func (c *Core) beforeLock(reason string) {
	if c.State() != Unlocked {
		return
	}
	c.saveEvents()
	_ = c.Vault.Update(func(d *vault.Data) error {
		d.LockedAt, d.LockReason = time.Now().UTC(), reason
		return nil
	})
}

// afterUnlock brings back what waited: saved events, sealed hooks, then tells each Bot about its backlog.
func (c *Core) afterUnlock() {
	var saved []events.Event
	var lockedAt time.Time
	var reason string
	_ = c.Vault.View(func(d *vault.Data) error {
		saved, lockedAt, reason = d.PendingEvents, d.LockedAt, d.LockReason
		return nil
	})
	c.Events.Restore(saved)
	c.ensureLockbox()
	if lockedAt.IsZero() || c.started.After(lockedAt) {
		lockedAt, reason = c.started, "Rubi restarted" // locked since this process started
	}
	unlockedAt := time.Now().UTC()
	go func() {
		hooks := c.replayHooks()
		time.Sleep(BacklogSettle) // let the replayed hooks and the plugins' catch-up emit their events
		c.noticeBacklog(lockedAt, unlockedAt, reason, hooks)
	}()
}

// BacklogSettle is how long after unlocking the backlog notices wait for catch-up events (tests shorten it).
var BacklogSettle = 20 * time.Second

// noticeBacklog tells each Bot with waiting events that Rubi was locked and how many wait for it.
func (c *Core) noticeBacklog(lockedAt, unlockedAt time.Time, reason string, hooks int) {
	if c.State() != Unlocked {
		return
	}
	var names []string
	_ = c.Vault.View(func(d *vault.Data) error {
		for _, a := range d.Agents {
			names = append(names, a.Name)
		}
		return nil
	})
	for _, name := range names {
		list, _ := c.eventsVisible(name, false)
		n := 0
		for _, e := range list {
			if !(e.Integration == "rubi" && e.Type == "backlog") {
				n++
			}
		}
		if n == 0 {
			continue
		}
		data := map[string]any{"locked_from": lockedAt, "unlocked_at": unlockedAt, "waiting": n, "reason": reason,
			"next_step": fmt.Sprintf("Rubi was locked from %s to %s (%s), so it couldn't act or watch meanwhile. %d events wait for you: read them with rubi_events, tell the user what matters, then rubi_ack each.",
				lockedAt.Format(time.RFC3339), unlockedAt.Format(time.RFC3339), orUnknown(reason), n)}
		if hooks > 0 {
			data["hooks_delivered_late"] = hooks
		}
		c.Events.EmitFor(name, false, "rubi", "backlog", data, nil)
	}
}

// ensureLockbox makes the lockbox key on first unlock and publishes its public half for locked times.
func (c *Core) ensureLockbox() {
	var key []byte
	_ = c.Vault.Update(func(d *vault.Data) error {
		if len(d.LockboxKey) != 32 {
			k, err := ecdh.X25519().GenerateKey(rand.Reader)
			if err != nil {
				return err
			}
			d.LockboxKey = k.Bytes()
		}
		key = d.LockboxKey
		return nil
	})
	priv, err := ecdh.X25519().NewPrivateKey(key)
	if err != nil {
		return
	}
	_ = vault.WriteFileAtomic(c.Layout.Lockbox(), priv.PublicKey().Bytes(), 0o600)
}

// keepHookWhileLocked seals a hook request that arrived while Rubi was locked, for the next unlock.
func (c *Core) keepHookWhileLocked(id string, body []byte) bool {
	pubBytes, err := os.ReadFile(c.Layout.Lockbox())
	if err != nil {
		return false
	}
	pub, err := ecdh.X25519().NewPublicKey(pubBytes)
	if err != nil {
		return false
	}
	plain, _ := json.Marshal(map[string]any{"id": id, "body": string(body), "at": time.Now().UTC()})
	req, _, err := e2e.Client{InstanceKey: pub}.Seal(lockboxPath, plain)
	if err != nil {
		return false
	}
	line, _ := json.Marshal(req)
	backlogMu.Lock()
	defer backlogMu.Unlock()
	if st, err := os.Stat(c.Layout.HooksWaiting()); err == nil && st.Size()+int64(len(line)) > maxWaitingBytes {
		return false
	}
	if n := countLines(c.Layout.HooksWaiting()); n >= maxWaitingHooks {
		return false
	}
	f, err := os.OpenFile(c.Layout.HooksWaiting(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err == nil
}

func countLines(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return bytes.Count(b, []byte{'\n'})
}

// replayHooks opens the hook requests kept while locked and hands each to its plugin once it runs.
func (c *Core) replayHooks() int {
	backlogMu.Lock()
	b, err := os.ReadFile(c.Layout.HooksWaiting())
	_ = os.Remove(c.Layout.HooksWaiting())
	backlogMu.Unlock()
	if err != nil || len(b) == 0 {
		return 0
	}
	var key []byte
	_ = c.Vault.View(func(d *vault.Data) error { key = d.LockboxKey; return nil })
	priv, err := ecdh.X25519().NewPrivateKey(key)
	if err != nil {
		return 0
	}
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var req e2e.Request
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		plain, _, err := e2e.Server{Key: priv}.Open(lockboxPath, req)
		if err != nil {
			continue
		}
		var in struct {
			ID   string `json:"id"`
			Body string `json:"body"`
		}
		if json.Unmarshal(plain, &in) != nil {
			continue
		}
		c.waitForHookPlugin(in.ID)
		if c.DeliverHook(in.ID, []byte(in.Body)) {
			n++
		}
	}
	if n > 0 {
		c.Audit.Record("hooks.replayed", audit.Fields{"count": n})
		log.Printf("delivered %d hook requests that arrived while Rubi was locked", n)
	}
	return n
}

// waitForHookPlugin waits (up to a minute) until the plugin owning hook id runs.
func (c *Core) waitForHookPlugin(id string) {
	var plugin string
	_ = c.Vault.View(func(d *vault.Data) error {
		if d.Hooks != nil {
			for _, h := range d.Hooks.List {
				if h.ID == id {
					plugin = h.Plugin
				}
			}
		}
		return nil
	})
	if plugin == "" {
		return
	}
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline) && !c.Runner.Running(plugin); {
		time.Sleep(250 * time.Millisecond)
	}
}

func orUnknown(reason string) string {
	if reason == "" {
		return "reason unknown"
	}
	return reason
}
