//go:build unix

package fsx

import (
	"context"
	"errors"
	"fmt"
	"net"
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

// AdminName is how the trusted owner of a file is called in messages.
const AdminName = "root"

// TrustedByAdmin says whether fi (what os.Stat saw at path) can be changed only by root: it is owned by root and
// neither the group nor others can write it. why says what is wrong when it is not.
func TrustedByAdmin(_ string, fi os.FileInfo) (ok bool, why string) {
	if uid, _, known := OwnerOf(fi); !known || uid != 0 {
		return false, "not owned by root"
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return false, fmt.Sprintf("writable by group or others (mode %04o)", fi.Mode().Perm())
	}
	return true, ""
}

// TrustedAncestor is TrustedByAdmin for a folder above the file: on Unix the same rule (a folder others can write
// lets them rename what is inside).
func TrustedAncestor(path string, fi os.FileInfo) (bool, string) { return TrustedByAdmin(path, fi) }

// ExplainTrust says whether a failed trust check gives a reason worth showing (on Unix the owner and the mode say it).
const ExplainTrust = false

// OthersName is who must not be able to write a trusted file.
const OthersName = "group or others"

// Reparse is false on Unix: a symbolic link is judged by its owner where it matters (see hookcheck.go).
func Reparse(string, os.FileInfo) bool { return false }

// FinalPath is not available on Unix; the caller compares the open file with the path (os.SameFile).
func FinalPath(*os.File) (string, error) { return "", ErrNoFinalPath }

// ListenLocal makes a local socket at path, mode 0600.
func ListenLocal(path string) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// DialLocalContext connects to a local socket.
func DialLocalContext(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

// IsSocket says whether a file is a socket.
func IsSocket(fi os.FileInfo) bool { return fi.Mode()&os.ModeSocket != 0 }

// StartTree is cmd.Start (a process group is made by KillTree).
func StartTree(cmd *exec.Cmd) error { return cmd.Start() }

// RunTree is cmd.Run (cancelling kills the process group KillTree made).
func RunTree(cmd *exec.Cmd) error { return cmd.Run() }

// WaitTree is cmd.Wait (on Windows it also lets go of the job StartTree made).
func WaitTree(cmd *exec.Cmd) error { return cmd.Wait() }

// ReleaseTree does nothing on Unix (on Windows it lets go of the job StartTree made).
func ReleaseTree(*exec.Cmd) {}

// AllowSocketRoot does nothing on Unix (on Windows it lets tests make sockets outside Home's folders).
func AllowSocketRoot(string) {}
