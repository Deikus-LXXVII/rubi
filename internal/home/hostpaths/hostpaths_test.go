package hostpaths

import (
	"path/filepath"
	"strings"
	"testing"
)

// Whatever the system, the lists are usable: home entries are names below the home folder, system entries are
// absolute, nothing is empty, and nothing is listed twice.
func TestListsAreWellFormed(t *testing.T) {
	uniq := func(name string, l []string) {
		t.Helper()
		if len(l) == 0 {
			t.Errorf("%s is empty", name)
		}
		seen := map[string]bool{}
		for _, e := range l {
			if e == "" || seen[strings.ToLower(e)] {
				t.Errorf("%s has an empty or repeated entry %q", name, e)
			}
			seen[strings.ToLower(e)] = true
		}
	}
	for name, l := range map[string][]string{"SensitiveHomeDirs": SensitiveHomeDirs(), "DeniedTrees": DeniedTrees()} {
		uniq(name, l)
		for _, e := range l {
			if filepath.IsAbs(e) || strings.ContainsAny(e, `/\`) {
				t.Errorf("%s: %q is not a name below the home folder", name, e)
			}
		}
	}
	for name, l := range map[string][]string{"SensitiveSysDirs": SensitiveSysDirs(), "ExactOnlyDirs": ExactOnlyDirs(),
		"SystemTrees": SystemTrees(), "SystemExact": SystemExact()} {
		uniq(name, l)
		for _, e := range l {
			if !filepath.IsAbs(e) {
				t.Errorf("%s: %q is not absolute", name, e)
			}
		}
	}
	for name, p := range map[string]string{"HookBinary": HookBinary(), "PluginDir": PluginDir(), "ManagedSettings": ManagedSettings()} {
		if !filepath.IsAbs(p) {
			t.Errorf("%s %q is not absolute", name, p)
		}
	}
	// the places the folder browser starts in depend on the disks mounted now: they may be none, and are absolute
	for _, r := range BrowseRoots() {
		if !filepath.IsAbs(r) {
			t.Errorf("BrowseRoots: %q is not absolute", r)
		}
	}
	if HelperHome("/h") == "" {
		t.Error("HelperHome is empty")
	}
}
