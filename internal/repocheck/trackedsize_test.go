package repocheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The release downloads node, rg and the rest into .runtime-cache; a commit once carried those tarballs by mistake.
// No tracked file may sit in that folder, and none may be large enough to be a downloaded or built binary.
func TestNoDownloadedOrLargeFilesTracked(t *testing.T) {
	base := root(t)
	out, err := exec.Command("git", "-C", base, "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	const limit = 1 << 20
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if rel == "" {
			continue
		}
		if strings.HasPrefix(rel, ".runtime-cache/") {
			t.Errorf("%s is a downloaded runtime source; it belongs in the ignored cache, not in git", rel)
			continue
		}
		info, err := os.Lstat(filepath.Join(base, rel))
		if err != nil {
			continue // removed in the work tree
		}
		if info.Size() > limit {
			t.Errorf("%s is %d bytes; tracked files stay under %d bytes", rel, info.Size(), limit)
		}
	}
}
