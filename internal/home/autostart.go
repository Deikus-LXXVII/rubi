package home

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Autostart starts Rubi Home when the user logs in and starts it again if it stops (docs/plans/home-platforms.md, stage
// P4). It is per user and never asks for administrator rights: if a system would need them, it says so and does
// nothing.
//
//	macOS    a LaunchAgent in ~/Library/LaunchAgents, through launchctl      (app_darwin.go, autostart_darwin.go)
//	Linux    a systemd user unit, through systemctl --user                   (autostart_systemd.go)
//	Windows  a Scheduled Task of the user that runs at their logon           (autostart_schtasks.go)
//
// Every form runs programs with argument lists; the files it writes are made from the installed binary's path and the
// log folder alone.
type Autostart interface {
	// Install writes the definition for spec.Bin, loads it if it is not loaded or has changed, and starts or restarts
	// it as that requires (a loaded agent restarts when spec.BinChanged says the binary was replaced).
	Install(ctx context.Context, spec AutostartSpec) (AutostartResult, error)
	// Restart stops the running Home and starts it again.
	Restart(ctx context.Context) error
	// Remove stops Home, takes it out of the start at login and deletes the definition.
	Remove(ctx context.Context) error
	// Loaded says whether the system has the definition loaded.
	Loaded(ctx context.Context) bool
}

// AutostartSpec is what Install is told.
type AutostartSpec struct {
	Bin        string // the installed binary
	Logs       string // the log folder (a form that has a log of its own, the journal, ignores it)
	BinChanged bool   // the binary was just replaced
}

// AutostartResult says what Install found and did.
type AutostartResult struct {
	WasLoaded bool // the definition was loaded before this call
	Restarted bool // this call started or restarted Home
}

// errAutostartWrite marks a failure to write the definition (as opposed to loading it), for the sentence the user sees.
var errAutostartWrite = errors.New("writing the start-at-login definition")

// ErrNoServiceManager is what a form returns when this user has no service manager to start Rubi Home (no systemd, or
// no user session to reach it) and none was set up before: Rubi Home's own supervisor is the way then
// (InstallAutostartOrSupervise).
var ErrNoServiceManager = errors.New("this computer has no service manager for this user")

// ErrNeedsElevation is what a form returns when the system would need administrator rights for what was asked: it asked
// for none and changed nothing.
var ErrNeedsElevation = errors.New("this needs administrator rights, which Rubi Home does not ask for")

// AutostartEnv is what a form of Autostart is made from; tests replace every part.
type AutostartEnv struct {
	Run         RunFunc
	Home        string // the user's home folder
	UID         int
	Logf        func(format string, args ...any)
	Sleep       func(time.Duration)
	CallTimeout time.Duration // one program call (20 s)
	LookPath    func(string) (string, error)
	SystemDir   string // Windows: System32 as the system names it
	User        string // Windows: the account name the task runs as (DOMAIN\name)
	TaskName    string // Windows: the task's name ("Rubi Home" when empty; a test uses another)
}

func (e AutostartEnv) withDefaults() AutostartEnv {
	if e.Run == nil {
		e.Run = execRun
	}
	if e.Logf == nil {
		e.Logf = func(string, ...any) {}
	}
	if e.Sleep == nil {
		e.Sleep = time.Sleep
	}
	if e.CallTimeout == 0 {
		e.CallTimeout = 20 * time.Second
	}
	if e.LookPath == nil {
		e.LookPath = lookPath
	}
	return e
}

// NewAutostart is the Autostart of the system Rubi Home is running on.
func NewAutostart(env AutostartEnv) Autostart { return platformAutostart(env.withDefaults()) }

// autostartFuncs makes an Autostart from four functions (the macOS form is written that way).
type autostartFuncs struct {
	install func(context.Context, AutostartSpec) (AutostartResult, error)
	restart func(context.Context) error
	remove  func(context.Context) error
	loaded  func(context.Context) bool
}

func (a autostartFuncs) Install(ctx context.Context, s AutostartSpec) (AutostartResult, error) {
	return a.install(ctx, s)
}
func (a autostartFuncs) Restart(ctx context.Context) error { return a.restart(ctx) }
func (a autostartFuncs) Remove(ctx context.Context) error  { return a.remove(ctx) }
func (a autostartFuncs) Loaded(ctx context.Context) bool   { return a.loaded(ctx) }

// run calls a program under the call timeout.
func (e AutostartEnv) run(ctx context.Context, stdin, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, e.CallTimeout)
	defer cancel()
	return e.Run(ctx, stdin, name, args...)
}

// removeFile deletes a file, and is happy when it is not there.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// stderrOf is what the program wrote on its error output, if err is its exit status.
func stderrOf(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(ee.Stderr)
	}
	return ""
}

// lookPath is exec.LookPath.
func lookPath(name string) (string, error) { return exec.LookPath(name) }

// writeFileReplace writes data to path (made with its folder) by a temporary file and a rename, so a reader never sees a
// half file and a link in the place is replaced by a regular file, never followed.
func writeFileReplace(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".rubi-home-new-*")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	ok = true
	return nil
}
