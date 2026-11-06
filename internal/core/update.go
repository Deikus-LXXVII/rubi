package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	handoff  string          // binary to exec into after an approved update (or a changed transport)
	reasons  map[string]bool // why Rubi restarts itself: restartUpdate, restartTransport (both can be pending)
	install  sync.Mutex      // one install of a release at a time (applyUpdate)
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
	if !info.Available || c.State() != Unlocked || c.QuietUpdates("rubi") {
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

// prepareRubiUpdate checks the release feed and the signature of the newest release, so that an approval
// screen states a checked fact. available is false when Rubi is up to date.
func (c *Core) prepareRubiUpdate(ctx context.Context) (info UpdateInfo, available bool, err error) {
	info = c.CheckForUpdate(ctx)
	if info.Error != "" && info.Latest == "" {
		return info, false, errors.New("couldn't check for updates: " + info.Error)
	}
	if !info.Available {
		return info, false, nil
	}
	if _, err := update.Checksums(ctx, info.Current, info.Latest); err != nil {
		return info, false, err
	}
	return info, true, nil
}

// rubiUpdatePreview is what the user sees about Rubi's own update.
func rubiUpdatePreview(info UpdateInfo) map[string]any {
	return map[string]any{"current": info.Current, "new_version": info.Latest, "release_notes": info.NotesURL,
		"verification": "Signed by the Rubi release key (checked). Rubi restarts and stays unlocked."}
}

// RequestUpdate prepares an approved, verified update to the latest release.
func (c *Core) RequestUpdate(ctx context.Context) (map[string]any, error) {
	if c.State() != Unlocked {
		return nil, errors.New("Rubi is locked; unlock it first")
	}
	info, available, err := c.prepareRubiUpdate(ctx)
	if err != nil {
		return nil, err
	}
	if !available {
		return map[string]any{"status": "up_to_date", "current": info.Current}, nil
	}
	return c.submitRubiUpdate(ctx, info)
}

func (c *Core) submitRubiUpdate(ctx context.Context, info UpdateInfo) (map[string]any, error) {
	target := info.Latest
	return c.Approvals.Submit(ctx, approvals.Request{
		Integration: "rubi", Kind: "rubi.update",
		Summary: "Update Rubi from " + info.Current + " to " + target,
		Preview: rubiUpdatePreview(info),
		Options: []approvals.Option{{Key: "update", Label: "Update"}},
		Execute: func(ctx context.Context, _ string) (any, error) { return c.applyUpdate(ctx, target) },
	})
}

func (c *Core) applyUpdate(ctx context.Context, target string) (any, error) {
	// One install at a time, and only while it is still an update: two Rubi approvals can wait together, and the
	// second one would install over the binary the first one put in place (and over its .prev, the way back).
	c.upd.install.Lock()
	defer c.upd.install.Unlock()
	c.upd.mu.Lock()
	armed := c.upd.reasons[restartUpdate] // a transport reason alone does not block: reasons add up
	c.upd.mu.Unlock()
	if armed || !update.Newer(target, version.Version) {
		return nil, errors.New("Rubi was updated or is restarting since you were asked; nothing was done")
	}
	install := c.InstallRelease
	if install == nil {
		install = installRelease
	}
	exe, err := install(ctx, target)
	if err != nil {
		return nil, err
	}
	c.Audit.Record("rubi.updated", audit.Fields{"from": version.Version, "to": target})
	if shutdown := c.armRestart(exe, restartUpdate); shutdown != nil {
		// Give the panel a moment to receive the result before the process restarts.
		time.AfterFunc(1500*time.Millisecond, shutdown)
	}
	return map[string]any{"status": "updated", "from": version.Version, "to": target, "restarting": true}, nil
}

// installRelease downloads the release, verifies it against the signed checksums and swaps it in for the
// running executable (the old one stays as .prev). It returns the executable to restart into.
func installRelease(ctx context.Context, target string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	v, err := update.Download(ctx, version.Version, target, filepath.Dir(exe))
	if err != nil {
		return "", err
	}
	if err := update.Install(v, exe); err != nil {
		return "", err
	}
	return exe, nil
}

// The reasons Rubi restarts itself into a binary with its vault key handed over.
const (
	restartUpdate    = "update"
	restartTransport = "transport"
)

// armRestart plans the restart into exe and records why; it returns the function that stops the daemon (nil when
// there is none). Reasons add up: if an update and a transport change are both waiting, the one restart carries both
// (it is the same binary, the new transport is already stored), so neither is lost and the new process reports both.
func (c *Core) armRestart(exe, reason string) (shutdown func()) {
	c.upd.mu.Lock()
	defer c.upd.mu.Unlock()
	c.upd.handoff = exe
	if c.upd.reasons == nil {
		c.upd.reasons = map[string]bool{}
	}
	c.upd.reasons[reason] = true
	return c.upd.shutdown
}

// HandoffReason says why the restart that is pending happens: "" for none, else "update", "transport" or
// "transport,update".
func (c *Core) HandoffReason() string {
	c.upd.mu.Lock()
	defer c.upd.mu.Unlock()
	var out []string
	for _, r := range []string{restartTransport, restartUpdate} {
		if c.upd.reasons[r] {
			out = append(out, r)
		}
	}
	return strings.Join(out, ",")
}

// PendingHandoff returns the binary to restart into, if an update was just installed.
func (c *Core) PendingHandoff() (string, bool) {
	c.upd.mu.Lock()
	defer c.upd.mu.Unlock()
	return c.upd.handoff, c.upd.handoff != ""
}
