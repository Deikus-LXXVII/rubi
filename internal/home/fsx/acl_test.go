package fsx

import (
	"os"
	"strings"
	"testing"
)

const (
	sidUsers         = "S-1-5-32-545"
	sidEveryone      = "S-1-1-0"
	sidAuthenticated = "S-1-5-11"
	sidMe            = "S-1-5-21-1111-2222-3333-1001"

	rRead  uint32 = 0x001200a9 // read and execute
	rFull  uint32 = 0x001f01ff // full control
	rAdd   uint32 = 0x00000004 // add a subfolder / append
	rDelCh uint32 = 0x00000040 // delete a child
)

func allow(sid string, mask uint32) ACE { return ACE{SID: sid, Kind: ACEAllow, Mask: mask} }

// What Program Files looks like: the trusted three have full control, Users only read.
func programFilesACL() []ACE {
	return []ACE{allow(SIDTrustedInstaller, rFull), allow(SIDSystem, rFull), allow(SIDAdministrators, rFull), allow(sidUsers, rRead)}
}

func TestCheckACL(t *testing.T) {
	inherit := func(a ACE) ACE { a.Flags = aceInheritOnly; return a }
	deny := func(sid string, mask uint32) ACE { return ACE{SID: sid, Kind: ACEDeny, Mask: mask} }
	for _, c := range []struct {
		name  string
		owner string
		null  bool
		aces  []ACE
		want  bool
		why   string // a part of the reason
	}{
		{"program files", SIDTrustedInstaller, false, programFilesACL(), true, ""},
		{"owned by Administrators", SIDAdministrators, false, programFilesACL(), true, ""},
		{"owned by SYSTEM", SIDSystem, false, programFilesACL(), true, ""},
		{"owned by the user", sidMe, false, programFilesACL(), false, "not owned by an administrator"},
		{"owned by Users", sidUsers, false, programFilesACL(), false, "not owned by an administrator"},
		{"without an access list", SIDAdministrators, true, nil, false, "without an access list"},
		{"Users may write", SIDAdministrators, false, append(programFilesACL(), allow(sidUsers, rFull)), false, "changeable by " + sidUsers},
		{"Everyone may write", SIDAdministrators, false, append(programFilesACL(), allow(sidEveryone, 0x2)), false, sidEveryone},
		{"Authenticated Users may append", SIDAdministrators, false, append(programFilesACL(), allow(sidAuthenticated, rAdd)), false, sidAuthenticated},
		{"the user may write", SIDAdministrators, false, append(programFilesACL(), allow(sidMe, 0x40000000)), false, sidMe},
		{"may change permissions", SIDAdministrators, false, append(programFilesACL(), allow(sidMe, 0x00040000)), false, sidMe},
		{"may take ownership", SIDAdministrators, false, append(programFilesACL(), allow(sidMe, 0x00080000)), false, sidMe},
		{"may delete", SIDAdministrators, false, append(programFilesACL(), allow(sidMe, 0x00010000)), false, sidMe},
		{"Users read only", SIDAdministrators, false, append(programFilesACL(), allow(sidEveryone, rRead)), true, ""},
		{"inherit-only write is not for this object", SIDAdministrators, false, append(programFilesACL(), inherit(allow(sidUsers, rFull))), true, ""},
		{"a deny entry is ignored", SIDAdministrators, false, append(programFilesACL(), deny(sidUsers, rFull)), true, ""},
		{"a deny does not undo an allow", SIDAdministrators, false, append(programFilesACL(), allow(sidUsers, rFull), deny(sidUsers, rFull)), false, sidUsers},
		{"an entry of an unknown kind", SIDAdministrators, false, append(programFilesACL(), ACE{SID: sidMe, Kind: ACEOther}), false, "not checked"},
		{"two bad trustees are both named", SIDAdministrators, false, append(programFilesACL(), allow(sidUsers, rFull), allow(sidMe, rFull)), false, sidMe},
	} {
		ok, why := CheckACL(c.owner, c.null, c.aces)
		if ok != c.want || !strings.Contains(why, c.why) || (c.want && why != "") {
			t.Errorf("%s: got %v %q, want %v %q", c.name, ok, why, c.want, c.why)
		}
	}
}

// A drive root lets Authenticated Users add folders; that does not let them replace Program Files.
func TestCheckAncestorACL(t *testing.T) {
	root := append(programFilesACL(), allow(sidAuthenticated, rAdd))
	if ok, why := CheckAncestorACL(SIDSystem, false, root); !ok {
		t.Errorf("a folder that only lets Authenticated Users add a subfolder: %q", why)
	}
	if ok, _ := CheckACL(SIDSystem, false, root); ok {
		t.Error("the same entries are fine for the file itself")
	}
	for name, mask := range map[string]uint32{"delete": 0x10000, "delete child": rDelCh, "write DACL": 0x40000, "owner": 0x80000, "generic all": 0x10000000} {
		if ok, _ := CheckAncestorACL(SIDSystem, false, append(programFilesACL(), allow(sidAuthenticated, mask))); ok {
			t.Errorf("%s on a folder above was accepted", name)
		}
	}
	if ok, _ := CheckAncestorACL(sidMe, false, programFilesACL()); ok {
		t.Error("a folder owned by the user was accepted")
	}
}

// DELETE on a folder above lets a trustee rename or remove it, and with it everything inside.
func TestAncestorDeleteIsUntrusted(t *testing.T) {
	ok, why := CheckAncestorACL(SIDSystem, false, append(programFilesACL(), allow(sidUsers, 0x10000)))
	if ok || !strings.Contains(why, sidUsers) {
		t.Errorf("Users allowed DELETE on an ancestor: %v %q", ok, why)
	}
}

func TestIsReparse(t *testing.T) {
	for _, c := range []struct {
		name  string
		mode  os.FileMode
		attrs uint32
		want  bool
	}{
		{"plain file", 0o644, 0x80, false},
		{"plain folder", os.ModeDir | 0o755, 0x10, false},
		{"symbolic link", os.ModeSymlink, 0, true},
		{"junction as Go reports it", os.ModeIrregular, 0, true},
		{"reparse attribute alone", os.ModeDir, 0x10 | 0x400, true},
		{"reparse attribute on a file", 0, 0x400, true},
		{"read-only and hidden", 0o444, 0x1 | 0x2, false},
	} {
		if got := IsReparse(c.mode, c.attrs); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestCheckVolume(t *testing.T) {
	const fixed, removable, remote, cdrom, ramdisk = 3, 2, 4, 5, 6
	for _, c := range []struct {
		name  string
		flags uint32
		drive uint32
		want  bool
		why   string
	}{
		{"NTFS fixed disk", 0x8 | 0x1 | 0x2, fixed, true, ""},
		{"NTFS on a removable drive", 0x8, removable, true, ""},
		{"network drive", 0x8, remote, false, "network drive"},
		{"FAT keeps no permissions", 0x1, fixed, false, "does not keep permissions"},
		{"unknown drive type", 0x8, 0, false, "unknown type"},
		{"no root directory", 0x8, 1, false, "unknown type"},
		{"RAM disk", 0x8, ramdisk, true, ""},
		{"read-only CD", 0x8, cdrom, true, ""},
		{"network drive with permissions flag off", 0, remote, false, "network drive"},
	} {
		ok, why := CheckVolume(c.flags, c.drive)
		if ok != c.want || !strings.Contains(why, c.why) {
			t.Errorf("%s: got %v %q", c.name, ok, why)
		}
	}
}
