package home

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
)

// An Elevator runs an installation plan with administrator rights, in the way the system asks for them
// (docs/plans/home-platforms.md, stage P5). The plan is the same everywhere (InstallPlan: the hook binary and, with it,
// the Claude Code plugin); what differs is the carrier:
//
//	macOS    osascript "do shell script ... with administrator privileges with prompt"   (elevate_darwin.go)
//	Linux    pkexec /bin/sh -c SCRIPT, with a polkit agent in the user's session         (elevate_pkexec.go)
//	Windows  a UAC prompt for the system's PowerShell running a constant bootstrap          (elevate_uac.go)
//
// Every form runs a program with an argument list. No form puts a path, a name or any other text of the user into a
// command line or into a script it composes from pieces: the macOS script and the Linux script are the plan's script, one
// argument that nothing parses but /bin/sh; the Windows command line holds a constant bootstrap with a path and a hash in
// base64, and the install script and the plan travel in files that the elevated side only uses when their SHA-256 is the one
// it was started with. That does not make a Windows command line tamper-proof: a process of the same user can interfere with the
// non-elevated side that starts the elevated one (see elevate_uac.go for what is and is not protected).
//
// The plan has a hash (PlanSHA256): the SHA-256 of the exact text root would run (FullScript). The approval in the panel
// shows it and `hook.install` is told it. Where nobody can be asked (Linux without a polkit agent) Home runs nothing and gives
// the Bot the exact argument list of the command, `/usr/bin/sudo -- /bin/sh -c SCRIPT`, which anyone can check:
// sha256(SCRIPT) is the hash the user approved. Rubi Home's own binary is never run as root.
type Elevator interface {
	// Describe is the "Run via:" line install-hook prints.
	Describe() string
	// Sentence is what the person at the computer will see, for the approval ("macOS will ask for ...").
	Sentence() string
	// Available says whether this computer can show the prompt now: nil, or ErrNoAgent where it cannot.
	Available(ctx context.Context) error
	// Run runs the plan as an administrator. ErrAdminCancelled when the person said no, ErrNoAgent when there was nobody to
	// ask.
	Run(ctx context.Context, p InstallPlan) error
}

// ErrNoAgent says there is no way to ask for the administrator here: no polkit agent, no desktop. On a Linux computer the
// installation then goes the headless way (`needs_bot_sudo`).
var ErrNoAgent = errors.New("there is no way to ask for the administrator password on this computer")

// PlanSHA256 is the hash of everything root would run: for the shell forms the whole script (the binary's hash, the paths,
// the plugin's files), for Windows the plan file. The approval shows it; the root side checks it.
func PlanSHA256(p InstallPlan) string {
	sum := sha256.Sum256([]byte(p.FullScript()))
	return hex.EncodeToString(sum[:])
}

// ElevExec runs a program with exactly this environment and returns its output (standard output and error together).
type ElevExec func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)

func execElev(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	return cmd.CombinedOutput()
}

// ElevateEnv is what a form of Elevator is made from; tests replace every part.
type ElevateEnv struct {
	Exec      ElevExec
	LookPath  func(string) (string, error)
	Getenv    func(string) string
	Geteuid   func() int
	SystemDir string // Windows: System32 as the system names it
	TempDir   func() (string, error)
}

func (e ElevateEnv) withDefaults() ElevateEnv {
	if e.Exec == nil {
		e.Exec = execElev
	}
	if e.LookPath == nil {
		e.LookPath = exec.LookPath
	}
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.Geteuid == nil {
		e.Geteuid = os.Geteuid
	}
	if e.TempDir == nil {
		e.TempDir = func() (string, error) { return os.MkdirTemp("", "rubi-home-plan-") }
	}
	return e
}

// NewElevator is the Elevator of the system Rubi Home is running on.
func NewElevator(env ElevateEnv) Elevator { return platformElevator(env.withDefaults()) }

// elevatorFuncs makes an Elevator from functions (the macOS form is written that way).
type elevatorFuncs struct {
	describe  string
	sentence  string
	available func(context.Context) error
	run       func(context.Context, InstallPlan) error
}

func (e elevatorFuncs) Describe() string { return e.describe }
func (e elevatorFuncs) Sentence() string { return e.sentence }
func (e elevatorFuncs) Available(ctx context.Context) error {
	if e.available == nil {
		return nil
	}
	return e.available(ctx)
}
func (e elevatorFuncs) Run(ctx context.Context, p InstallPlan) error { return e.run(ctx, p) }

func trimOut(out []byte, err error) string {
	s := string(out)
	if len(s) > 2000 {
		s = s[:2000]
	}
	if s == "" {
		return err.Error()
	}
	return err.Error() + ": " + s
}
