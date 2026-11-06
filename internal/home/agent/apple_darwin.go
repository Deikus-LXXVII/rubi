package agent

import (
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

// The macOS facts of Apple's container tool (docs/plans/home-platforms.md, stage P8): where its installer puts it, what it is
// called, and that the macOS backend is the one this system uses. The rest of the Apple backend (apple.go, apple_backend.go) is
// plain Go over these.

// installedBin is where Apple's installer puts the CLI; it is preferred to whatever PATH names.
var installedBin = "/usr/local/bin/container"

// ContainerBin is the container CLI: where Apple's installer puts it, else "container" in PATH (a launchd
// job's PATH doesn't have /usr/local/bin, and a PATH entry isn't trusted over the installed one).
func ContainerBin() string {
	if st, err := os.Stat(installedBin); err == nil && st.Mode().IsRegular() {
		return installedBin
	}
	if p, err := exec.LookPath("container"); err == nil {
		return p
	}
	return "container"
}

// ExecToolName is the name of the program a T3 session's exec command line runs ("container"): the agent-vm shim only runs
// a command Home sent if it is this tool's exec.
func ExecToolName() string { return "container" }

// appleSilicon says whether this Mac's CPU is Apple silicon. It asks the kernel about the hardware, not about this program,
// so an Intel build that Rosetta runs on Apple silicon still counts.
var appleSilicon = func() bool {
	v, err := unix.SysctlUint32("hw.optional.arm64")
	return err == nil && v == 1
}

// noAppleSilicon is why an Intel Mac has no agents: Apple's container tool runs only on Apple silicon.
const noAppleSilicon = "agents need a Mac with Apple silicon"

func platformBackend(cfg BackendConfig) VMBackend {
	if !appleSilicon() {
		return NoBackend(noAppleSilicon)
	}
	return NewAppleBackend(cfg)
}
