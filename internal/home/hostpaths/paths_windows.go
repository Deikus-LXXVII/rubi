package hostpaths

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// known is a known folder of the system (not an environment variable, which the user can change), or fallback.
func known(id *windows.KNOWNFOLDERID, fallback string) string {
	if p, err := windows.KnownFolderPath(id, windows.KF_FLAG_DEFAULT); err == nil && p != "" {
		return p
	}
	return fallback
}

func localAppData(home string) string {
	return known(windows.FOLDERID_LocalAppData, filepath.Join(home, "AppData", "Local"))
}

func programFiles() string { return known(windows.FOLDERID_ProgramFiles, `C:\Program Files`) }

// DataBase is %LOCALAPPDATA%: Home's keys must stay on this computer, which %AppData% (roaming) would not.
func DataBase() (string, error) {
	home, _ := os.UserHomeDir()
	return localAppData(home), nil
}

// HelperHome is the folder of the installed copy of Home and its logs: %LOCALAPPDATA%\Rubi Home.
func HelperHome(home string) string { return filepath.Join(localAppData(home), "Rubi Home") }

// HookBinary is the administrator-owned copy of rubi-home that Claude Code's managed hooks run.
func HookBinary() string { return filepath.Join(programFiles(), "Rubi Home", "rubi-home.exe") }

// PluginDir is where the Claude Code marketplace is installed, and what managed settings point at.
func PluginDir() string { return filepath.Join(programFiles(), "Rubi Home", "claude-plugin") }

// ManagedSettings is the file Claude Code reads for managed settings.
func ManagedSettings() string {
	return filepath.Join(programFiles(), "ClaudeCode", "managed-settings.json")
}

// SensitiveHomeDirs are the folders below the home folder a session is never started in.
func SensitiveHomeDirs() []string {
	return []string{".claude", ".ssh", ".gnupg", ".aws", ".kube", ".config", ".docker", "AppData", "Application Data",
		"Local Settings", "Cookies"}
}

func winDir() string { return known(windows.FOLDERID_Windows, `C:\Windows`) }

func drive() string { return filepath.VolumeName(winDir()) + `\` }

// SensitiveSysDirs are the system folders a session is never started in, nor anything inside them.
func SensitiveSysDirs() []string {
	return []string{winDir(), programFiles(), known(windows.FOLDERID_ProgramFilesX86, `C:\Program Files (x86)`),
		known(windows.FOLDERID_ProgramData, `C:\ProgramData`), drive() + "System Volume Information", drive() + `$Recycle.Bin`,
		drive() + "Recovery"}
}

// ExactOnlyDirs are refused as a working folder themselves, but not what is inside them.
func ExactOnlyDirs() []string { return []string{drive() + "Users"} }

// SystemTrees are never a project, nor anything inside them.
func SystemTrees() []string { return SensitiveSysDirs() }

// SystemExact are never a project themselves; what is inside them may be.
func SystemExact() []string { return []string{drive() + "Users", drive()} }

// DeniedTrees are folders under the user's home that never become a project, nor anything inside them.
func DeniedTrees() []string {
	return []string{".ssh", ".gnupg", ".aws", ".config", ".kube", "AppData"}
}

// BrowseRoots are the places besides the home folder where the folder browser may start: the local drives that are
// not the system drive.
func BrowseRoots() []string {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	system := strings.ToUpper(filepath.VolumeName(winDir()))
	var out []string
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		letter := string(rune('A' + i))
		root := letter + `:\`
		if strings.ToUpper(letter+":") == system {
			continue
		}
		p, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		if t := windows.GetDriveType(p); t == windows.DRIVE_FIXED || t == windows.DRIVE_REMOVABLE {
			out = append(out, root)
		}
	}
	return out
}
