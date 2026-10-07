//go:build !linux

package plugins

import "syscall"

// Plugins run in their own process group. (Only Linux can tie their lifetime to the daemon's.)
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
