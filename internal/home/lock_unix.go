//go:build darwin || linux || freebsd || netbsd || openbsd

package home

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock (flock) on path, creating it; the returned function releases it.
// flock locks belong to the open file, so it also orders two Helpers of one process.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
