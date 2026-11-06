package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/hostpaths"
)

// mklinkJ makes a junction; skips the test where that is not possible.
func mklinkJ(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skip("no junctions here:", err, string(out))
	}
}

// A junction is not listed (Go reports it as irregular), and naming one resolves it to the folder it leads to, which is
// checked as that folder: out of the root, or into a denied place, it is refused.
func TestWindowsJunctionsAreNotListedAndAreChecked(t *testing.T) {
	e, home := folderEnv(t)
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	private := filepath.Join(home, hostpaths.DeniedTrees()[0])
	mk(t, filepath.Join(home, "real"), filepath.Join(outside, "elsewhere"), private)
	mklinkJ(t, filepath.Join(home, "j-real"), filepath.Join(home, "real"))
	mklinkJ(t, filepath.Join(home, "j-out"), filepath.Join(outside, "elsewhere"))
	mklinkJ(t, filepath.Join(home, "j-private"), private)
	l, err := e.s.ListFolders("")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(l), ","); got != "real" {
		t.Fatalf("listed %q: no junction at all", got)
	}
	for _, j := range []string{"j-out", "j-private"} {
		if l, err := e.s.ListFolders(filepath.Join(home, j)); err == nil {
			t.Errorf("%s was listed: %+v", j, l)
		}
	}
	// a junction to a folder of the root resolves to that folder, and is listed as it
	l, err = e.s.ListFolders(filepath.Join(home, "j-real"))
	if err != nil || !strings.EqualFold(l.Path, filepath.Join(home, "real")) {
		t.Errorf("junction to a folder of the root: %+v %v", l, err)
	}
}

// The opened folder's final path is checked: a junction put in place of the folder after the check is caught.
func TestWindowsJunctionSwappedAfterTheCheck(t *testing.T) {
	e, home := folderEnv(t)
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	asked := filepath.Join(home, "work")
	mk(t, asked, filepath.Join(outside, "elsewhere"))
	f, err := os.Open(asked)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.verifyOpened(f, asked, home, home); err != nil {
		t.Fatalf("the folder as it was: %v", err)
	}
	f.Close()
	os.Remove(asked)
	mklinkJ(t, asked, filepath.Join(outside, "elsewhere"))
	f, err = os.Open(asked)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := e.s.verifyOpened(f, asked, home, home); err == nil {
		t.Error("a junction out of the root went unnoticed")
	}
}
