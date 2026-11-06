package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The Linux VM's root file system is read-only, so every place a share is mounted outside the project's own /work
// share must be made when the image is built (runner/microvm/rootfs-dirs.txt): a role VM's mount table needs nothing
// the image doesn't have.
func TestMicroVMRootfsHasEveryMountPoint(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "runner", "microvm", "rootfs-dirs.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			dirs = append(dirs, l)
		}
	}
	p := Project{Dir: "/p", Role: "r_aaaaaa01", EmptyDir: "/e", RoleDir: "/r", ClaudeDir: "/c",
		ReadOnly: []ShieldMount{{Name: ".claude", Source: "/s1"}, {Name: ".git/hooks", Source: "/s2"}}}
	for _, m := range p.Mounts() {
		if m.Target == "/work" || !strings.HasPrefix(m.Target, "/work/") {
			if !slices.Contains(dirs, m.Target) {
				t.Errorf("%s is mounted on but not made in the image", m.Target)
			}
		}
	}
}
