//go:build !darwin

package agent

import "runtime"

// Linux and Windows have no VM backend yet (a QEMU/KVM microvm for Linux is stage P9 of docs/plans/home-platforms.md), so there
// is no container tool to name and the backend is "none", with the reason.

// ContainerBin is empty where there is no container tool.
func ContainerBin() string { return "" }

// ExecToolName is empty where there is no tool whose exec command line the agent-vm shim could run.
func ExecToolName() string { return "" }

func platformBackend(BackendConfig) VMBackend {
	switch runtime.GOOS {
	case "linux":
		return NoBackend("agents don't run on Linux yet")
	case "windows":
		return NoBackend("agents don't run on Windows yet")
	}
	return NoBackend("agents don't run on this system yet")
}
