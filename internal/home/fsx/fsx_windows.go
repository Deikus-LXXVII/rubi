//go:build windows

package fsx

import (
	"errors"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

// NonBlock does not exist on Windows.
const NonBlock = 0

// OpenNoFollow is os.OpenFile that refuses a path whose last element is a link (a symlink or a junction). The
// path is looked at before it is opened and the opened file is compared with what was seen, so a swap in between
// is caught. (Windows has no O_NOFOLLOW; a handle opened with FILE_FLAG_OPEN_REPARSE_POINT is the stronger form,
// for stage P6.)
func OpenNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	before, err := os.Lstat(path)
	switch {
	case err == nil:
		if before.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return nil, &os.PathError{Op: "open", Path: path, Err: ErrLink}
		}
	case errors.Is(err, os.ErrNotExist):
		before = nil
	default:
		return nil, err
	}
	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	if before != nil {
		if after, serr := f.Stat(); serr != nil || !os.SameFile(before, after) {
			f.Close()
			return nil, &os.PathError{Op: "open", Path: path, Err: ErrLink}
		}
	}
	return f, nil
}

func lock(f *os.File, flags uint32) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|flags, 0, 1, 0, new(windows.Overlapped))
}

// Lock takes an exclusive lock on the first byte of f (LockFileEx), waiting for it.
func Lock(f *os.File) error { return lock(f, 0) }

// TryLock is Lock that does not wait: ErrLocked when someone else holds it.
func TryLock(f *os.File) error {
	err := lock(f, windows.LOCKFILE_FAIL_IMMEDIATELY)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrLocked
	}
	return err
}

// Unlock releases a lock taken by Lock or TryLock (closing the file does too).
func Unlock(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

// OwnerOf reports no owner on Windows: the caller must treat the file as not owned by an administrator.
func OwnerOf(os.FileInfo) (uint32, uint64, bool) { return 0, 0, false }

// LinkCount is the number of hard links of an open file (NumberOfLinks).
func LinkCount(f *os.File) (uint64, bool) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return 0, false
	}
	return uint64(info.NumberOfLinks), true
}

// KillTree does nothing yet: cancelling kills the process but not its children (a Job Object, stage P6).
func KillTree(*exec.Cmd) {}
