//go:build windows

package fsx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// NonBlock does not exist on Windows.
const NonBlock = 0

// OpenNoFollow is os.OpenFile that refuses a path whose last element is a link of any kind (a symbolic link, a junction, any
// reparse point). Windows has no O_NOFOLLOW; the file is opened with FILE_FLAG_OPEN_REPARSE_POINT, which opens the link itself
// and does not follow it, and the handle's own attributes are then read: a reparse point is closed again and refused. There is
// no gap between a look at the path and the open, because the decision is made on what was opened.
//
// The open never truncates (NoFollowPlan): O_TRUNC is carried out through the handle after the check, so a link planted at the
// path can't get its target emptied. Sharing is read and write, as os.OpenFile does (not delete). Only files are opened; a
// directory would need FILE_FLAG_BACKUP_SEMANTICS, which is left out on purpose (a directory fails to open).
func OpenNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	access, disp, truncate, attrs := NoFollowPlan(flag, perm)
	h, err := windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, disp,
		attrs|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if IsReparse(0, info.FileAttributes) {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: ErrLink}
	}
	f := os.NewFile(uintptr(h), path)
	if truncate {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, &os.PathError{Op: "truncate", Path: path, Err: err}
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
	owner, null, aces, err := readACL(path)
	if err != nil {
		return false, "unreadable (" + err.Error() + ")"
	}
	return check(owner, null, aces)
}

// readACL reads the owner and the access list of a file or folder.
func readACL(path string) (owner string, nullDACL bool, aces []ACE, err error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", false, nil, fmt.Errorf("its permissions can't be read: %w", err)
	}
	o, _, err := sd.Owner()
	if err != nil || o == nil {
		return "", false, nil, errors.New("its owner can't be read")
	}
	owner = o.String()
	dacl, _, err := sd.DACL()
	if err != nil {
		if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
			return owner, true, nil, nil
		}
		return "", false, nil, fmt.Errorf("its access list can't be read: %w", err)
	}
	if dacl == nil {
		return owner, true, nil, nil
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return "", false, nil, fmt.Errorf("its access list can't be read: %w", err)
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
	return owner, false, aces, nil
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

// FinalPath is the path the system says an open file really is, after every link and junction on the way
// (GetFinalPathNameByHandle), in the ordinary form. It is what makes a check made on a path hold for the file that
// was opened.
func FinalPath(f *os.File) (string, error) {
	h := windows.Handle(f.Fd())
	buf := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if int(n) <= len(buf) {
			return TrimExtendedPrefix(windows.UTF16ToString(buf[:n])), nil
		}
		buf = make([]uint16, n+1)
	}
}
