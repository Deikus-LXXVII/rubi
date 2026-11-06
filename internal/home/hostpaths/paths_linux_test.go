package hostpaths

import (
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestLinuxDataBaseIsTheXDGConfigFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if got, err := DataBase(); err != nil || got != home+"/.config" {
		t.Errorf("without XDG_CONFIG_HOME: %q %v", got, err)
	}
	t.Setenv("XDG_CONFIG_HOME", home+"/xdg")
	if got, err := DataBase(); err != nil || got != home+"/xdg" {
		t.Errorf("with XDG_CONFIG_HOME: %q %v", got, err)
	}
}

func TestLinuxPaths(t *testing.T) {
	if HookBinary() != "/usr/local/bin/rubi-home" || HelperHome("/home/a") != "/home/a/.rubi-home" ||
		ManagedSettings() != "/etc/claude-code/managed-settings.json" || PluginDir() != "/usr/local/share/rubi-home/claude-plugin" {
		t.Errorf("%q %q %q %q", HookBinary(), HelperHome("/home/a"), ManagedSettings(), PluginDir())
	}
}

// The macOS entries are gone from the Linux lists, the Linux ones are in, and what Linux used before stays denied.
// The roots are the disks this user has mounted under /run/media/<user> and /media/<user>: not /mnt (on WSL /mnt/c is the
// Windows system drive), not /srv, not the folder that holds the disks.
func TestLinuxBrowseRootsAreTheUsersMountedDisks(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	old := mediaBases
	mediaBases = func() []string { return []string{a, b, filepath.Join(a, "missing")} }
	t.Cleanup(func() { mediaBases = old })
	os.Mkdir(filepath.Join(a, "disk1"), 0o755)
	os.Mkdir(filepath.Join(b, "disk2"), 0o755)
	os.WriteFile(filepath.Join(b, "file"), nil, 0o644)
	os.Symlink("/", filepath.Join(a, "link-to-root"))
	want := []string{filepath.Join(a, "disk1"), filepath.Join(b, "disk2")}
	if got := BrowseRoots(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// the real bases name this user and nothing else
	mediaBases = old
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	if got := mediaBases(); !reflect.DeepEqual(got, []string{"/run/media/" + u.Username, "/media/" + u.Username}) {
		t.Errorf("bases %v", got)
	}
	for _, r := range BrowseRoots() {
		if r == "/mnt" || strings.HasPrefix(r, "/mnt/") || strings.HasPrefix(r, "/srv") {
			t.Errorf("root %q", r)
		}
	}
}

func TestLinuxListsHaveNoMacFolders(t *testing.T) {
	for _, l := range [][]string{SensitiveHomeDirs(), DeniedTrees(), SensitiveSysDirs(), SystemTrees(), SystemExact(), ExactOnlyDirs()} {
		for _, bad := range []string{"Library", "/Library", "/System", "/private", "/Applications", "/Volumes", "/Users", "/cores", "/private/etc", "/private/var"} {
			if slices.Contains(l, bad) {
				t.Errorf("%v has the macOS entry %q", l, bad)
			}
		}
	}
	for _, want := range []string{".ssh", ".gnupg", ".aws", ".config", ".kube"} {
		if !slices.Contains(DeniedTrees(), want) {
			t.Errorf("DeniedTrees lost %q", want)
		}
	}
	for _, want := range []string{"/etc", "/usr", "/bin", "/proc", "/sys", "/var", "/opt", "/run", "/boot", "/root"} {
		if !slices.Contains(SensitiveSysDirs(), want) {
			t.Errorf("SensitiveSysDirs lacks %q", want)
		}
	}
	for _, want := range []string{".cache", ".password-store", ".ssh", ".claude"} {
		if !slices.Contains(SensitiveHomeDirs(), want) {
			t.Errorf("SensitiveHomeDirs lacks %q", want)
		}
	}
}
