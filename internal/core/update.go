package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/audit"
	"github.com/Deikus-LXXVII/rubi/internal/update"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

// UpdateInfo is what the agent and the panel see about new releases.
type UpdateInfo struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest,omitempty"`
	Available bool      `json:"available"`
	NotesURL  string    `json:"notes_url,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type updateState struct {
	mu       sync.Mutex
	info     UpdateInfo
	handoff  string // binary to exec into after an approved update
	shutdown func()
}

// SetShutdown lets the daemon provide the function that stops it gracefully (used to restart into an update).
func (c *Core) SetShutdown(fn func()) {
	c.upd.mu.Lock()
	c.upd.shutdown = fn
	c.upd.mu.Unlock()
}

// UpdateInfo returns the latest known release information.
func (c *Core) UpdateInfo() UpdateInfo {
	c.upd.mu.Lock()
	defer c.upd.mu.Unlock()
	info := c.upd.info
	info.Current = version.Version
	return info
}

// CheckForUpdate reads the release feed and, when a newer release exists, tells the agent once per version.
func (c *Core) CheckForUpdate(ctx context.Context) UpdateInfo {
	rel, err := update.Latest(ctx, version.Version)
	c.upd.mu.Lock()
	c.upd.info.CheckedAt = time.Now().UTC()
	if err != nil {
		c.upd.info.Error = err.Error()
	} else {
		c.upd.info.Error = ""
		c.upd.info.Latest, c.upd.info.NotesURL = rel.Version, rel.NotesURL
		c.upd.info.Available = update.Newer(rel.Version, version.Version)
	}
	c.upd.mu.Unlock()
	c.maybeNotifyUpdate()
	return c.UpdateInfo()
}

// maybeNotifyUpdate emits rubi.update.available once per version, as soon as Rubi is unlocked (events go to
// the agent's webhook, which is stored in the vault).
func (c *Core) maybeNotifyUpdate() {
	info := c.UpdateInfo()
	if !info.Available || c.State() != Unlocked {
		return
	}
	already := false
	_ = c.Vault.View(func(d *vault.Data) error {
		already = d.UpdateNotified == info.Latest
		return nil
	})
	if already {
		return
	}
	_ = c.Vault.Update(func(d *vault.Data) error {
		d.UpdateNotified = info.Latest
		return nil
	})
	c.Events.Emit("rubi", "update.available", map[string]any{"current": info.Current, "latest": info.Latest,
		"notes_url": info.NotesURL,
		"how":       "Tell the user a Rubi update is available. If they want it, call rubi_update; they approve it in the panel."}, nil)
}

// RequestUpdate prepares an approved, verified update to the latest release.
func (c *Core) RequestUpdate(ctx context.Context) (map[string]any, error) {
	if c.State() != Unlocked {
		return nil, errors.New("Rubi is locked; unlock it first")
	}
	info := c.CheckForUpdate(ctx)
	if info.Error != "" && info.Latest == "" {
		return nil, errors.New("couldn't check for updates: " + info.Error)
	}
	if !info.Available {
		return map[string]any{"status": "up_to_date", "current": info.Current}, nil
	}
	// Verify the release signature before asking the user, so the approval screen states a checked fact.
	if _, err := update.Checksums(ctx, info.Current, info.Latest); err != nil {
		return nil, err
	}
	target := info.Latest
	return c.Approvals.Submit(ctx, approvals.Request{
		Integration: "rubi", Kind: "rubi.update",
		Summary: "Update Rubi from " + info.Current + " to " + target,
		Preview: map[string]any{"current": info.Current, "new_version": target, "release_notes": info.NotesURL,
			"verification": "Signed by the Rubi release key (checked). Rubi restarts and stays unlocked."},
		Options: []approvals.Option{{Key: "update", Label: "Update"}},
		Execute: func(ctx context.Context, _ string) (any, error) { return c.applyUpdate(ctx, target) },
	})
}

func (c *Core) applyUpdate(ctx context.Context, target string) (any, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	v, err := update.Download(ctx, version.Version, target, filepath.Dir(exe))
	if err != nil {
		return nil, err
	}
	if err := update.Install(v, exe); err != nil {
		return nil, err
	}
	c.Audit.Record("rubi.updated", audit.Fields{"from": version.Version, "to": target})
	c.upd.mu.Lock()
	c.upd.handoff = exe
	shutdown := c.upd.shutdown
	c.upd.mu.Unlock()
	if shutdown != nil {
		// Give the panel a moment to receive the result before the process restarts.
		time.AfterFunc(1500*time.Millisecond, shutdown)
	}
	return map[string]any{"status": "updated", "from": version.Version, "to": target, "restarting": true}, nil
}

// PendingHandoff returns the binary to restart into, if an update was just installed.
func (c *Core) PendingHandoff() (string, bool) {
	c.upd.mu.Lock()
	defer c.upd.mu.Unlock()
	return c.upd.handoff, c.upd.handoff != ""
}
