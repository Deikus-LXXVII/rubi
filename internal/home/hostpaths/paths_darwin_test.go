package hostpaths

import (
	"reflect"
	"testing"
)

// macOS keeps exactly what Rubi Home always used: these are the literals of the code before stage P2.
func TestMacValuesAreUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, err := DataBase(); err != nil || got != home+"/Library/Application Support" {
		t.Errorf("DataBase %q %v", got, err)
	}
	for name, c := range map[string][2]string{
		"HelperHome":      {HelperHome("/Users/a"), "/Users/a/.rubi-home"},
		"HookBinary":      {HookBinary(), "/usr/local/bin/rubi-home"},
		"PluginDir":       {PluginDir(), "/Library/Application Support/Rubi Home/claude-plugin"},
		"ManagedSettings": {ManagedSettings(), "/Library/Application Support/ClaudeCode/managed-settings.json"},
	} {
		if c[0] != c[1] {
			t.Errorf("%s: got %q, want %q", name, c[0], c[1])
		}
	}
	for name, c := range map[string][2][]string{
		"SensitiveHomeDirs": {SensitiveHomeDirs(), {".claude", ".ssh", ".gnupg", ".aws", ".kube", ".config", ".docker", ".local", ".npm", ".oh-my-zsh", "Library"}},
		"SensitiveSysDirs": {SensitiveSysDirs(), {"/etc", "/private/etc", "/System", "/Library", "/usr", "/bin", "/sbin", "/dev", "/proc", "/sys",
			"/var", "/private/var", "/Applications", "/opt"}},
		"ExactOnlyDirs": {ExactOnlyDirs(), {"/Volumes"}},
		"SystemTrees": {SystemTrees(), {"/System", "/Library", "/usr", "/bin", "/sbin", "/etc", "/var", "/private", "/dev", "/cores",
			"/Applications", "/tmp"}},
		"SystemExact": {SystemExact(), {"/Volumes", "/Users", "/home", "/opt"}},
		"DeniedTrees": {DeniedTrees(), {".ssh", "Library", ".gnupg", ".aws", ".config", ".kube"}},
	} {
		if !reflect.DeepEqual(c[0], c[1]) {
			t.Errorf("%s: got %v, want %v", name, c[0], c[1])
		}
	}
}
