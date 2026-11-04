//go:build unix

package fsx

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// NonBlock is O_NONBLOCK: opening a FIFO or a device must not hang. It is 0 where the flag does not exist.
const NonBlock = syscall.O_NONBLOCK

// OpenNoFollow is os.OpenFile that fails when the last element of path is a symbolic link (O_NOFOLLOW).
func OpenNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
}

// Lock takes an exclusive advisory lock (flock) on f, waiting for it. The lock belongs to the open file, so it
// also orders two holders in one process.
func Lock(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// TryLock is Lock that does not wait: ErrLocked when someone else holds it.
func TryLock(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return ErrLocked
		}
		return err
	}
}

// Unlock releases a lock taken by Lock or TryLock (closing the file does too).
func Unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// OwnerOf is the owner (uid) and inode of a file, from its FileInfo; ok is false when the system does not say.
func OwnerOf(fi os.FileInfo) (uid uint32, ino uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return st.Uid, uint64(st.Ino), true
}

// LinkCount is the number of hard links of an open file; ok is false when the system does not say.
func LinkCount(f *os.File) (n uint64, ok bool) {
	fi, err := f.Stat()
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}

// KillTree makes cmd start in a group of its own, and cancelling its context kill the whole group.
func KillTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
