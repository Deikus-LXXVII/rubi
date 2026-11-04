package fsx

import "os"

// How OpenNoFollow asks Windows to open a file (the values are those of the Win32 API, so that the mapping can be tested on
// every host). The one rule that matters: the open never creates-or-truncates in the same step that could follow a link. A
// file is opened (or created new) without truncation; the handle is looked at for a reparse point; only then is it truncated,
// through the handle. Truncating in the open itself (CREATE_ALWAYS, TRUNCATE_EXISTING) would empty the target of a link
// planted at the path before the look.
const (
	win32GenericRead    = 0x80000000
	win32GenericWrite   = 0x40000000
	win32FileAppendData = 0x00000004
	win32CreateNew      = 1
	win32OpenAlways     = 4
	win32OpenExisting   = 3
	win32AttrNormal     = 0x80
	win32AttrReadonly   = 0x01
)

// NoFollowPlan maps os.OpenFile's flag and perm to the access wanted, the disposition (never one that truncates), whether the
// handle must be truncated after the check, and the attributes of a file made new.
func NoFollowPlan(flag int, perm os.FileMode) (access, disp uint32, truncate bool, attrs uint32) {
	switch flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_WRONLY:
		access = win32GenericWrite
	case os.O_RDWR:
		access = win32GenericRead | win32GenericWrite
	default:
		access = win32GenericRead
	}
	if flag&os.O_APPEND != 0 && access&win32GenericWrite != 0 {
		access = access&^win32GenericWrite | win32FileAppendData
	}
	switch {
	case flag&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL:
		disp = win32CreateNew
	case flag&os.O_CREATE != 0:
		disp = win32OpenAlways
	default:
		disp = win32OpenExisting
	}
	truncate = flag&os.O_TRUNC != 0
	attrs = win32AttrNormal
	if perm&0o200 == 0 {
		attrs = win32AttrReadonly
	}
	return
}
