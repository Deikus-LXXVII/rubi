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
	// Home is the user's home folder from the account database, not $HOME: the daemon is started by a Bot's MCP
	// session, so its environment is the Bot's, and nothing in it may choose what the daemon runs.
	Home string
	UID  int
	// Getenv reads the few harmless variables passed on (LANG, XDG_RUNTIME_DIR).
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

// Binary is the rubi-home install-home.sh puts in place by default (~/.rubi-home/bin/rubi-home). An install moved
// elsewhere with $RUBI_HOME_INSTALL isn't started by the daemon.
func (e Env) Binary() string { return filepath.Join(hostpaths.HelperHome(e.Home), "bin", "rubi-home") }

// configDir is Rubi Home's default folder on Linux (~/.config/Rubi Home); a Rubi Home given another folder with
// $RUBI_HOME_DIR or $XDG_CONFIG_HOME isn't started by the daemon, and the run it starts uses the default.
func (e Env) configDir() string { return filepath.Join(e.Home, ".config", "Rubi Home") }

// ErrNotInstalled says there is no Rubi Home set up for this user here: nothing to start.
var ErrNotInstalled = errors.New("no Rubi Home is set up for this user")

// trusted refuses a binary the daemon must not run: not a plain file, or one that anyone but this user (and root)
// could have written or swapped. That covers the file and every folder above it, followed through symlinks, so no
// one else can replace the binary between this check and its start. A root-owned folder that others may write
// (such as /tmp) counts only with its sticky bit, which keeps them from renaming what isn't theirs.
func trusted(path string, uid int) error {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	for p := real; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("can't read the owner")
		}
		if p == real && !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a plain file", p)
		}
		owner := int(st.Uid)
		writable := fi.Mode().Perm()&0o022 != 0
		sticky := fi.Mode()&os.ModeSticky != 0
		switch {
		case owner != uid && owner != 0:
			return fmt.Errorf("%s belongs to another user", p)
		case writable && !(owner == 0 && sticky && p != real):
			return fmt.Errorf("%s could be changed by someone other than this user", p)
		}
		if parent := filepath.Dir(p); parent == p {
			return nil
		}
	}
}

// minimalEnv is the environment Rubi Home gets: its home folder and a plain PATH, and nothing of the process that
// started the daemon (a Bot's MCP session, whose environment may hold its secrets).
func (e Env) minimalEnv() []string {
	env := []string{"HOME=" + e.Home, "PATH=/usr/local/bin:/usr/bin:/bin"}
	for _, k := range []string{"LANG", "XDG_RUNTIME_DIR"} {
		if v := e.getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Ensure has the installed Rubi Home make sure it runs. ErrNotInstalled when there is none set up.
func Ensure(ctx context.Context, e Env) error {
	if e.Home == "" {
		return ErrNotInstalled
	}
	if _, err := os.Stat(filepath.Join(e.configDir(), "identity.json")); err != nil {
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
