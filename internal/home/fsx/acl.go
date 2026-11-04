package fsx

import (
	"fmt"
	"os"
	"strings"
)

// The Windows form of "only root can change this file" is a rule about its owner and its access list (DACL). The rule
// is plain logic over strings and numbers, so it lives here, in a file that builds everywhere and is tested
// everywhere; fsx_windows.go only reads the descriptor from the system and hands it over.

// Well-known security identifiers.
const (
	SIDAdministrators   = "S-1-5-32-544"
	SIDSystem           = "S-1-5-18"
	SIDTrustedInstaller = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
)

const aceInheritOnly uint8 = 0x08

// trustedSIDs may own a file that counts as an administrator's, and may be given the right to change it.
var trustedSIDs = map[string]bool{SIDAdministrators: true, SIDSystem: true, SIDTrustedInstaller: true}

// ACEKind is what an access control entry does.
type ACEKind int

const (
	ACEAllow ACEKind = iota // grants the rights in Mask
	ACEDeny                 // takes rights away; it is ignored (ignoring it can only make the check stricter)
	ACEOther                // a kind this check does not understand: never trusted
)

// ACE is one entry of a DACL.
type ACE struct {
	SID   string
	Kind  ACEKind
	Mask  uint32
	Flags uint8 // the ACE header's flags; INHERIT_ONLY_ACE (0x08) entries do not apply to the object itself
}

// writeRights are the access bits that let a trustee change a file or a folder: write data or add files, append or
// add folders, write extended attributes and attributes, delete, delete a child, change the permissions, take
// ownership, and the generic write and all rights.
const writeRights uint32 = 0x00000002 | 0x00000004 | 0x00000010 | 0x00000040 | 0x00000100 |
	0x00010000 | 0x00040000 | 0x00080000 | 0x10000000 | 0x40000000

// replaceRights are the bits that let a trustee replace or remove an existing child of a folder, or move or take the
// folder: delete a child, delete the folder itself (renaming it needs DELETE), change the permissions, take ownership, generic write and all rights. Adding a file or a folder
// is not among them, and the drive root gives Authenticated Users exactly that.
const replaceRights uint32 = 0x00000040 | 0x00010000 | 0x00040000 | 0x00080000 | 0x10000000 | 0x40000000

// CheckACL says whether a file with this owner and these entries can be changed only by an administrator: it is
// owned by Administrators, SYSTEM or TrustedInstaller, there is an access list, and no entry that applies to the
// file itself gives a write right to anybody else (Users, Everyone, Authenticated Users, or the user who runs
// Rubi Home). why names the reason when it is not.
func CheckACL(owner string, nullDACL bool, aces []ACE) (ok bool, why string) {
	return checkACL(owner, nullDACL, aces, writeRights)
}

// CheckAncestorACL is CheckACL for a folder above the file: only the rights to replace what is inside count.
func CheckAncestorACL(owner string, nullDACL bool, aces []ACE) (ok bool, why string) {
	return checkACL(owner, nullDACL, aces, replaceRights)
}

func checkACL(owner string, nullDACL bool, aces []ACE, rights uint32) (ok bool, why string) {
	if !trustedSIDs[owner] {
		return false, "not owned by an administrator (owner " + owner + ")"
	}
	if nullDACL {
		return false, "without an access list, so everyone can change it"
	}
	var bad []string
	for _, a := range aces {
		if a.Flags&aceInheritOnly != 0 {
			continue
		}
		switch a.Kind {
		case ACEDeny:
			continue
		case ACEOther:
			return false, "with an access rule of a kind that is not checked (for " + a.SID + ")"
		}
		if a.Mask&rights != 0 && !trustedSIDs[a.SID] {
			bad = append(bad, a.SID)
		}
	}
	if len(bad) > 0 {
		return false, fmt.Sprintf("changeable by %s", strings.Join(bad, ", "))
	}
	return true, ""
}

const (
	fileAttributeReparsePoint uint32 = 0x400
	filePersistentACLs        uint32 = 0x8
	driveUnknown              uint32 = 0
	driveNoRootDir            uint32 = 1
	driveRemote               uint32 = 4
)

// IsReparse says whether a file is a link of any kind: a symbolic link, a junction or another reparse point. Go
// reports a junction as ModeIrregular, and the attribute bit catches what Go does not name. A caller that follows a
// reparse point trusts the permissions of the place it leads to, not of the place it was told about.
func IsReparse(mode os.FileMode, attrs uint32) bool {
	return mode&(os.ModeSymlink|os.ModeIrregular) != 0 || attrs&fileAttributeReparsePoint != 0
}

// CheckVolume says whether the permissions on a volume mean anything: the file system keeps them
// (FILE_PERSISTENT_ACLS) and the volume is not a network drive or unknown. flags are those of GetVolumeInformation
// and driveType is the result of GetDriveType.
func CheckVolume(flags, driveType uint32) (ok bool, why string) {
	switch {
	case driveType == driveRemote:
		return false, "on a network drive"
	case driveType == driveUnknown || driveType == driveNoRootDir:
		return false, "on a drive of an unknown type"
	case flags&filePersistentACLs == 0:
		return false, "on a file system that does not keep permissions"
	}
	return true, ""
}
