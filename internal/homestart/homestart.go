//go:build unix

// Package homestart has the Rubi daemon keep Rubi Home running on a computer with no service manager for its user (a
// shared Linux box, docs/plans/t3-cloud-agents.md slice 2c). The daemon itself is started lazily there: any MCP use
// brings it up (internal/mcpproxy). While it runs it asks the installed Rubi Home to make sure it runs too
// (`rubi-home start`, which does nothing when Rubi Home already runs or the system starts it), so the first Bot
// session after a computer update brings back both, and with them the T3 host.
package homestart

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/hostpaths"
)

// Env is what Ensure looks at; the zero values of the funcs are the real thing.
type Env struct {
	Home   string // the user's home folder
	UID    int
	Getenv func(string) string
	// Run runs bin with args and env, bounded by ctx, and returns its combined output.
	Run func(ctx context.Context, bin string, args, env []string) ([]byte, error)
}

func (e Env) getenv(k string) string {
	if e.Getenv != nil {
		return e.Getenv(k)
	}
	return os.Getenv(k)
}

// Binary is the rubi-home the installer put in place for this user (install-home.sh: $RUBI_HOME_INSTALL, else
// ~/.rubi-home, then bin/rubi-home).
func (e Env) Binary() string {
	dir := e.getenv("RUBI_HOME_INSTALL")
	if dir == "" {
		dir = hostpaths.HelperHome(e.Home)
	}
	return filepath.Join(dir, "bin", "rubi-home")
}

// configDir is Rubi Home's folder (home.DefaultDir: $RUBI_HOME_DIR, else the user's config folder).
func (e Env) configDir() (string, error) {
	if d := e.getenv("RUBI_HOME_DIR"); d != "" {
		return d, nil
	}
	if x := e.getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "Rubi Home"), nil
	}
	if e.Home == "" {
		return "", errors.New("no home folder")
	}
	return filepath.Join(e.Home, ".config", "Rubi Home"), nil
}

// ErrNotInstalled says there is no Rubi Home set up for this user here: nothing to start.
var ErrNotInstalled = errors.New("no Rubi Home is set up for this user")

// trusted refuses a binary the daemon must not run: not a plain file, or one that another user, or a group, could
// have written, or in such a folder. The daemon holds the keys; it runs only what only this user could have put there.
func trusted(path string, uid int) error {
	for p := path; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("can't read the owner")
		}
		if p == path && !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a plain file", p)
		}
		if int(st.Uid) != uid || fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s could be changed by someone other than this user", p)
		}
		if p == filepath.Dir(path) { // the binary and its folder; the folders above are the user's home and its parents
			return nil
		}
	}
}

// minimalEnv is the environment Rubi Home gets: what it needs to find its folders, and nothing of the process that
// started the daemon (on a box that is a Bot's MCP session, whose environment may hold its secrets).
func (e Env) minimalEnv() []string {
	env := []string{"HOME=" + e.Home, "PATH=/usr/local/bin:/usr/bin:/bin"}
	for _, k := range []string{"USER", "LOGNAME", "LANG", "TMPDIR", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "RUBI_HOME_DIR"} {
		if v := e.getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Ensure has the installed Rubi Home make sure it runs. ErrNotInstalled when there is none set up.
func Ensure(ctx context.Context, e Env) error {
	dir, err := e.configDir()
	if err != nil {
		return ErrNotInstalled
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err != nil {
		return ErrNotInstalled
	}
	bin := e.Binary()
	if _, err := os.Lstat(bin); err != nil {
		return ErrNotInstalled
	}
	if err := trusted(bin, e.UID); err != nil {
		return fmt.Errorf("not starting Rubi Home: %w", err)
	}
	run := e.Run
	if run == nil {
		run = func(ctx context.Context, bin string, args, env []string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, bin, args...)
			cmd.Env, cmd.Dir = env, "/"
			return cmd.CombinedOutput()
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if out, err := run(ctx, bin, []string{"start"}, e.minimalEnv()); err != nil {
		return fmt.Errorf("rubi-home start: %w: %s", err, clip(string(out)))
	}
	return nil
}

func clip(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// Every is how often Keep checks again (the supervisor restarts Rubi Home itself; this brings back a supervisor
// that was killed).
const Every = 15 * time.Minute

// Keep runs Ensure now and every Every until ctx ends, logging only changes.
func Keep(ctx context.Context, e Env, logf func(string, ...any)) {
	last := "unset"
	for {
		err := Ensure(ctx, e)
		msg := ""
		switch {
		case errors.Is(err, ErrNotInstalled):
			msg = "none"
		case err != nil:
			msg = err.Error()
		}
		if msg != last && msg != "none" {
			if msg == "" {
				logf("Rubi Home: running")
			} else {
				logf("Rubi Home: %s", msg)
			}
		}
		last = msg
		select {
		case <-ctx.Done():
			return
		case <-time.After(Every):
		}
	}
}
