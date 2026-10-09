// Package paths defines Rubi's on-disk layout.
//
// Everything lives under one directory ($RUBI_HOME, default ~/.rubi), created with mode 0700.
// Only non-secret material is stored unencrypted: the instance identity (needed before unlock),
// the wrapped data keys, and the sealed vault itself.
package paths

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type Layout struct {
	Home string
}

func Default() (Layout, error) {
	if h := os.Getenv("RUBI_HOME"); h != "" {
		return Layout{Home: h}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Layout{}, err
	}
	return Layout{Home: filepath.Join(home, ".rubi")}, nil
}

func (l Layout) Ensure() error {
	if err := os.MkdirAll(l.Home, 0o700); err != nil {
		return err
	}
	return os.Chmod(l.Home, 0o700)
}

func (l Layout) Identity() string { return filepath.Join(l.Home, "identity.json") }
func (l Layout) Keys() string     { return filepath.Join(l.Home, "keys.json") }

// Lockbox is the public key that seals hook requests arriving while Rubi is locked; HooksWaiting holds
// them, sealed, until the next unlock (core/backlog.go).
func (l Layout) Lockbox() string      { return filepath.Join(l.Home, "lockbox.pub") }
func (l Layout) HooksWaiting() string { return filepath.Join(l.Home, "hooks-waiting") }

// Watch holds the watch key and the registrations sealed to the watchers (core/watchdog.go).
func (l Layout) Watch() string { return filepath.Join(l.Home, "watch.json") }

// PanelOrigin records the panel origin this instance was paired with (see core.Open).
func (l Layout) PanelOrigin() string { return filepath.Join(l.Home, "panel-origin") }
func (l Layout) Vault() string       { return filepath.Join(l.Home, "vault.sealed") }

// Socket is the daemon's Unix socket. Unix socket paths are limited to ~104 bytes, so a long $RUBI_HOME
// falls back to a short per-home name in a private per-user directory ($XDG_RUNTIME_DIR, or
// /tmp/rubi-<uid> created 0700 and checked to be ours). A shared, predictable path in /tmp could be taken
// first by someone else, who would then pose as Rubi to the agent.
func (l Layout) Socket() string {
	p := filepath.Join(l.Home, "rubi.sock")
	if len(p) <= 100 {
		return p
	}
	sum := sha256.Sum256([]byte(l.Home))
	name := fmt.Sprintf("rubi-%x.sock", sum[:6])
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" && privateDir(d) && len(filepath.Join(d, name)) <= 100 {
		return filepath.Join(d, name)
	}
	dir := fmt.Sprintf("/tmp/rubi-%d", os.Getuid())
	_ = os.Mkdir(dir, 0o700)
	if !privateDir(dir) {
		return p // too long to bind: the daemon fails loudly instead of using a directory others control
	}
	return filepath.Join(dir, name)
}

// privateDir reports whether dir is a real directory (not a link) owned by this user with no access for
// anyone else.
func privateDir(dir string) bool {
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
func (l Layout) Lock() string      { return filepath.Join(l.Home, "daemon.lock") }
func (l Layout) DaemonLog() string { return filepath.Join(l.Home, "daemon.log") }
func (l Layout) Audit() string     { return filepath.Join(l.Home, "audit.log") }
func (l Layout) Plugins() string   { return filepath.Join(l.Home, "plugins") }
func (l Layout) Logs() string      { return filepath.Join(l.Home, "logs") }

// OpenLog opens a log file for appending (0600). Logs are plain text on disk, so they are kept small:
// past maxLog the file is moved to <name>.1 (one old file is kept) and a new one begins.
func OpenLog(path string) (*os.File, error) {
	if st, err := os.Stat(path); err == nil && st.Size() > maxLog {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

const maxLog = 5 << 20
