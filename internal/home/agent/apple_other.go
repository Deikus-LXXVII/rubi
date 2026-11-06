//go:build !darwin

package agent

import "runtime"

// Linux runs agents' VMs with the microvm backend (QEMU/KVM, stage P9 of docs/plans/home-platforms.md); Windows has no VM
// backend yet. Neither has a container tool to name.

// ContainerBin is empty where there is no container tool.
func ContainerBin() string { return "" }

// ExecToolName is empty where there is no tool whose exec command line the agent-vm shim could run.
func ExecToolName() string { return "" }

func platformBackend(cfg BackendConfig) VMBackend {
	switch runtime.GOOS {
	case "linux":
		return linuxBackend(cfg)
	case "windows":
		return NoBackend("agents don't run on Windows yet")
	}
	return NoBackend("agents don't run on this system yet")
}
