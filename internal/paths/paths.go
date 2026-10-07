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
func (l Layout) Vault() string    { return filepath.Join(l.Home, "vault.sealed") }

// Socket is the daemon's Unix socket. Unix socket paths are limited to ~104 bytes, so a long $RUBI_HOME
// falls back to a short per-user, per-home path in /tmp (the socket itself is mode 0600).
func (l Layout) Socket() string {
	p := filepath.Join(l.Home, "rubi.sock")
	if len(p) <= 100 {
		return p
	}
	sum := sha256.Sum256([]byte(l.Home))
	return fmt.Sprintf("/tmp/rubi-%d-%x.sock", os.Getuid(), sum[:6])
}
func (l Layout) Lock() string      { return filepath.Join(l.Home, "daemon.lock") }
func (l Layout) DaemonLog() string { return filepath.Join(l.Home, "daemon.log") }
func (l Layout) Audit() string     { return filepath.Join(l.Home, "audit.log") }
func (l Layout) Plugins() string   { return filepath.Join(l.Home, "plugins") }
func (l Layout) Logs() string      { return filepath.Join(l.Home, "logs") }
