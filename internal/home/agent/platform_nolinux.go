//go:build !darwin && !linux

package agent

func linuxBackend(BackendConfig) VMBackend { return NoBackend("agents don't run on Linux yet") }
