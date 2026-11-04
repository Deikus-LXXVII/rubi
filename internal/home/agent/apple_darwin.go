package agent

import (
	"os"
	"os/exec"
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

func platformBackend(cfg BackendConfig) VMBackend { return NewAppleBackend(cfg) }
