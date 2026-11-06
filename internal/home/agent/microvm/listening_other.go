//go:build !linux

package microvm

import "os"

// listening: without /proc/net/unix, the socket's file is taken for its listening (the VM backend is Linux's alone).
func listening(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}
