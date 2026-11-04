package hostpaths

import (
	"os"
	"path/filepath"
)

// DataBase is the user's configuration folder: ~/Library/Application Support.
func DataBase() (string, error) { return os.UserConfigDir() }

// HelperHome is the folder of the installed copy of Home and its logs, in the user's home folder.
func HelperHome(home string) string { return filepath.Join(home, ".rubi-home") }

// HookBinary is the root-owned copy of rubi-home that Claude Code's managed hooks run.
func HookBinary() string { return "/usr/local/bin/rubi-home" }

// PluginDir is where the Claude Code marketplace is installed, and what managed settings point at.
func PluginDir() string { return "/Library/Application Support/Rubi Home/claude-plugin" }

// ManagedSettings is the file Claude Code reads for managed settings.
func ManagedSettings() string {
	return "/Library/Application Support/ClaudeCode/managed-settings.json"
}

// SensitiveHomeDirs are the folders below the home folder a session is never started in.
func SensitiveHomeDirs() []string {
	return []string{".claude", ".ssh", ".gnupg", ".aws", ".kube", ".config", ".docker", ".local", ".npm", ".oh-my-zsh", "Library"}
}

// SensitiveSysDirs are the system folders a session is never started in, nor anything inside them.
func SensitiveSysDirs() []string {
	return []string{"/etc", "/private/etc", "/System", "/Library", "/usr", "/bin", "/sbin", "/dev", "/proc", "/sys",
		"/var", "/private/var", "/Applications", "/opt"}
}

// ExactOnlyDirs are refused as a working folder themselves, but not what is inside them.
func ExactOnlyDirs() []string { return []string{"/Volumes"} }

// SystemTrees are never a project, nor anything inside them.
func SystemTrees() []string {
	return []string{"/System", "/Library", "/usr", "/bin", "/sbin", "/etc", "/var", "/private", "/dev", "/cores", "/Applications", "/tmp"}
}

// SystemExact are never a project themselves; what is inside them may be.
func SystemExact() []string { return []string{"/Volumes", "/Users", "/home", "/opt"} }

// DeniedTrees are folders under the user's home that never become a project, nor anything inside them.
func DeniedTrees() []string { return []string{".ssh", "Library", ".gnupg", ".aws", ".config", ".kube"} }
