package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/hostpaths"
)

// A project is never one of the platform's denied folders in the user's home, nor inside one; ~/Library is
// denied on a Mac only.
func TestProtectedDirFollowsThePlatform(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, d := range append(hostpaths.DeniedTrees(), "Library", "work") {
		if err := os.MkdirAll(filepath.Join(home, d, "p"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range hostpaths.DeniedTrees() {
		if !ProtectedDir(filepath.Join(home, d, "p")) || !ProtectedDir(filepath.Join(home, d)) {
			t.Errorf("%s in the home folder can be a project", d)
		}
	}
	if lib := ProtectedDir(filepath.Join(home, "Library", "p")); lib != (runtime.GOOS == "darwin") {
		t.Errorf("~/Library denied = %v on %s", lib, runtime.GOOS)
	}
	if ProtectedDir(filepath.Join(home, "work", "p")) {
		t.Error("an ordinary folder is protected")
	}
}
