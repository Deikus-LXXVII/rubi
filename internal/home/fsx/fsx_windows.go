//go:build windows

package fsx

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"unsafe"

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

// AdminName is how the trusted owner of a file is called in messages.
const AdminName = "an administrator"

// TrustedByAdmin says whether the file at path can be changed only by an administrator: its owner is Administrators,
// SYSTEM or TrustedInstaller and its access list gives nobody else a write right (CheckACL). Anything it can not
// read counts as not trusted.
func TrustedByAdmin(path string, _ os.FileInfo) (bool, string) { return trusted(path, CheckACL) }

// TrustedAncestor is TrustedByAdmin for a folder above the file: only a right to replace what is in it counts.
func TrustedAncestor(path string, _ os.FileInfo) (bool, string) {
	return trusted(path, CheckAncestorACL)
}

func trusted(path string, check func(string, bool, []ACE) (bool, string)) (bool, string) {
	if li, err := os.Lstat(path); err != nil {
		return false, "unreadable (" + err.Error() + ")"
	} else if Reparse(path, li) {
		return false, "a link or a junction"
	}
	if ok, why := volumeOK(path); !ok {
		return false, why
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, "unreadable (its permissions can't be read: " + err.Error() + ")"
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return false, "unreadable (its owner can't be read)"
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
			return check(owner.String(), true, nil)
		}
		return false, "unreadable (its access list can't be read: " + err.Error() + ")"
	}
	if dacl == nil {
		return check(owner.String(), true, nil)
	}
	var aces []ACE
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, "unreadable (its access list can't be read: " + err.Error() + ")"
		}
		e := ACE{Mask: uint32(ace.Mask), Flags: ace.Header.AceFlags}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			e.Kind = ACEAllow
		case windows.ACCESS_DENIED_ACE_TYPE:
			e.Kind = ACEDeny
		default:
			e.Kind = ACEOther
		}
		if e.Kind != ACEOther {
			e.SID = (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		}
		aces = append(aces, e)
	}
	return check(owner.String(), false, aces)
}

// ExplainTrust says whether a failed trust check gives a reason worth showing: on Windows the owner and mode of a
// file mean nothing, so the reason is all there is.
const ExplainTrust = true

// OthersName is who must not be able to write a trusted file.
const OthersName = "anyone else"

// Reparse says whether path (as Lstat saw it) is a symbolic link, a junction or another reparse point.
func Reparse(path string, li os.FileInfo) bool {
	var mode os.FileMode
	if li != nil {
		mode = li.Mode()
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return true
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return true // unreadable counts as a link: fail closed
	}
	return IsReparse(mode, attrs)
}

// volumeOK checks the volume path is on (CheckVolume).
func volumeOK(path string) (bool, string) {
	vol := filepath.VolumeName(path)
	if vol == "" {
		return false, "on no known volume"
	}
	root, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return false, "on no known volume"
	}
	var flags uint32
	if err := windows.GetVolumeInformation(root, nil, 0, nil, nil, &flags, nil, 0); err != nil {
		return false, "on a volume that can't be read (" + err.Error() + ")"
	}
	return CheckVolume(flags, windows.GetDriveType(root))
}
