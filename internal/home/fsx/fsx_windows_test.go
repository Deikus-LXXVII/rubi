package fsx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// There is no owner on Windows: every caller must then treat the file as not an administrator's.
func TestOwnerOfSaysUnknown(t *testing.T) {
	fi, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := OwnerOf(fi); ok {
		t.Fatal("Windows reported an owner")
	}
}

// The system folder is owned by the system and Users can not write it; a folder of the test is the user's.
func TestTrustedByAdminOnWindows(t *testing.T) {
	sys := os.Getenv("SystemRoot")
	if sys == "" {
		t.Skip("no SystemRoot")
	}
	fi, err := os.Stat(sys + `\System32\cmd.exe`)
	if err != nil {
		t.Skip(err)
	}
	if ok, why := TrustedByAdmin(sys+`\System32\cmd.exe`, fi); !ok {
		t.Errorf("cmd.exe: %s", why)
	}
	dir := t.TempDir()
	fi, _ = os.Stat(dir)
	if ok, _ := TrustedByAdmin(dir, fi); ok {
		t.Error("a temporary folder of the user was trusted")
	}
	if _, why := TrustedByAdmin(dir+`\nowhere`, fi); why == "" {
		t.Error("a missing path was trusted")
	}
}

func systemRoot(t *testing.T) string {
	t.Helper()
	sys := os.Getenv("SystemRoot")
	if sys == "" {
		t.Skip("no SystemRoot")
	}
	return sys
}

// setDACL replaces the access list of path with a protected one: the given entries, nothing inherited.
func setDACL(t *testing.T, path string, grants map[string]windows.ACCESS_MASK) {
	t.Helper()
	var entries []windows.EXPLICIT_ACCESS
	for sidText, mask := range grants {
		sid, err := windows.StringToSid(sidText)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: mask,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid)},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Skip("can't set the access list here:", err)
	}
}

// A folder with only Administrators and SYSTEM in its list is trusted (when the test runs as an administrator, so that
// the owner is Administrators); an explicit DELETE for Users makes it untrusted, as a folder above the hook.
func TestAncestorWithUsersDeleteIsUntrusted(t *testing.T) {
	dir := t.TempDir()
	setDACL(t, dir, map[string]windows.ACCESS_MASK{SIDAdministrators: 0x1f01ff, SIDSystem: 0x1f01ff})
	fi, _ := os.Stat(dir)
	if ok, why := TrustedAncestor(dir, fi); !ok {
		t.Skip("the folder is not an administrator's here, so the test can't start from a trusted one:", why)
	}
	setDACL(t, dir, map[string]windows.ACCESS_MASK{SIDAdministrators: 0x1f01ff, SIDSystem: 0x1f01ff, "S-1-5-32-545": 0x10000})
	ok, why := TrustedAncestor(dir, fi)
	if ok || !strings.Contains(why, "S-1-5-32-545") {
		t.Errorf("Users with DELETE: %v %q", ok, why)
	}
	if ok, _ := TrustedByAdmin(dir, fi); ok {
		t.Error("Users with DELETE: the folder itself was trusted")
	}
}

// The drive root lets Authenticated Users add folders; for a folder above the hook that is acceptable.
func TestDriveRootIsAcceptableAsAnAncestor(t *testing.T) {
	root := filepath.VolumeName(systemRoot(t)) + `\`
	fi, err := os.Stat(root)
	if err != nil {
		t.Skip(err)
	}
	if ok, why := TrustedAncestor(root, fi); !ok {
		t.Errorf("%s as an ancestor: %s", root, why)
	}
}

func TestJunctionIsRefused(t *testing.T) {
	sys := systemRoot(t)
	link := filepath.Join(t.TempDir(), "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, sys).CombinedOutput(); err != nil {
		t.Skip("no junctions here:", err, string(out))
	}
	li, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !Reparse(link, li) {
		t.Error("a junction was not taken for a link")
	}
	fi, _ := os.Stat(link)
	if ok, why := TrustedByAdmin(link, fi); ok || !strings.Contains(why, "junction") {
		t.Errorf("the junction to the Windows folder: %v %q", ok, why)
	}
	if ok, _ := TrustedAncestor(link, fi); ok {
		t.Error("a junction as an ancestor was accepted")
	}
	// the real folder is not a link, and the system drive keeps permissions
	li, _ = os.Lstat(sys)
	if Reparse(sys, li) {
		t.Error("the Windows folder was taken for a link")
	}
	if ok, why := volumeOK(sys); !ok {
		t.Errorf("the system drive: %s", why)
	}
}
