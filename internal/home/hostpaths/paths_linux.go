package hostpaths

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// DataBase is the user's configuration folder: $XDG_CONFIG_HOME, else ~/.config.
func DataBase() (string, error) { return os.UserConfigDir() }

// HelperHome is the folder of the installed copy of Home and its logs, in the user's home folder.
func HelperHome(home string) string { return filepath.Join(home, ".rubi-home") }

// HookBinary is the root-owned copy of rubi-home that Claude Code's managed hooks run.
func HookBinary() string { return "/usr/local/bin/rubi-home" }

// PluginDir is where the Claude Code marketplace is installed, and what managed settings point at.
func PluginDir() string { return "/usr/local/share/rubi-home/claude-plugin" }

// ManagedSettings is the file Claude Code reads for managed settings.
func ManagedSettings() string { return "/etc/claude-code/managed-settings.json" }

// SensitiveHomeDirs are the folders below the home folder a session is never started in.
func SensitiveHomeDirs() []string {
	return []string{".claude", ".ssh", ".gnupg", ".aws", ".kube", ".config", ".docker", ".local", ".npm", ".oh-my-zsh",
		".cache", ".password-store", ".mozilla", ".var"}
}

// SensitiveSysDirs are the system folders a session is never started in, nor anything inside them.
func SensitiveSysDirs() []string {
	return []string{"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/dev", "/proc", "/sys", "/run", "/boot",
		"/root", "/snap", "/var", "/opt"}
}

// ExactOnlyDirs are refused as a working folder themselves, but not what is inside them.
func ExactOnlyDirs() []string { return []string{"/mnt", "/media"} }

// SystemTrees are never a project, nor anything inside them.
func SystemTrees() []string {
	return []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/etc", "/var", "/dev", "/proc", "/sys", "/run",
		"/boot", "/root", "/snap", "/tmp"}
}

// SystemExact are never a project themselves; what is inside them may be.
func SystemExact() []string { return []string{"/home", "/opt", "/mnt", "/media", "/srv"} }

// DeniedTrees are folders under the user's home that never become a project, nor anything inside them.
func DeniedTrees() []string {
	return []string{".ssh", ".gnupg", ".aws", ".config", ".kube", ".password-store"}
}

// mediaBases are the folders where a desktop mounts removable disks for this user (a variable so that a test can point
// it elsewhere). Not /mnt (WSL's /mnt/c is the Windows system drive), and not /srv.
var mediaBases = func() []string {
	u, err := user.Current()
	if err != nil || u.Username == "" || strings.ContainsAny(u.Username, `/\`) || u.Username == "." || u.Username == ".." {
		return nil
	}
	return []string{filepath.Join("/run/media", u.Username), filepath.Join("/media", u.Username)}
}

// BrowseRoots are the places besides the home folder where the folder browser may start: each removable disk this user
// has mounted, /run/media/<user>/<disk> or /media/<user>/<disk>.
func BrowseRoots() []string {
	var out []string
	for _, base := range mediaBases() {
		ents, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() {
				out = append(out, filepath.Join(base, e.Name()))
			}
		}
	}
	return out
}
