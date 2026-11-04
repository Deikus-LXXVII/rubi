package hostpaths

import (
	"slices"
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
