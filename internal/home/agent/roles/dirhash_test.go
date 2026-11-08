package roles

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirHashCanonicalAliasesAndRetargeting(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	os.Mkdir(a, 0700)
	os.Mkdir(b, 0700)
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(a, alias); err != nil {
		t.Skip(err)
	}
	first := DirHash(alias)
	if first == "" || first != DirHash(a) || first != DirHash(filepath.Join(alias, ".")) {
		t.Fatal("aliases do not share canonical binding")
	}
	os.Remove(alias)
	os.Symlink(b, alias)
	if DirHash(alias) == first || DirHash(alias) != DirHash(b) {
		t.Fatal("symlink retarget retained old binding")
	}
	for _, path := range []string{"", "relative", filepath.Join(root, "missing")} {
		if DirHash(path) != "" {
			t.Fatal("unresolved path acquired a binding")
		}
	}
}
