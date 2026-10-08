package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLongHomeSocketIsPrivate(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	l := Layout{Home: filepath.Join(t.TempDir(), strings.Repeat("x", 120))}
	p := l.Socket()
	if len(p) > 104 {
		t.Fatalf("socket path too long: %s", p)
	}
	dir := filepath.Dir(p)
	if !privateDir(dir) {
		t.Fatalf("socket directory %s is not private", dir)
	}
	// A directory others can enter is never used.
	open := t.TempDir()
	_ = os.Chmod(open, 0o755)
	if privateDir(open) {
		t.Fatal("a world-readable directory counted as private")
	}
}
