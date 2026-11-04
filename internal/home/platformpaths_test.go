package home

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/hostpaths"
)

// Home's folder is where it always was: "Rubi Home" in the user's configuration folder (macOS, Linux), whatever
// hostpaths does for the other systems; RUBI_HOME_DIR still wins.
func TestDefaultDirIsWhereItAlwaysWas(t *testing.T) {
	t.Setenv("RUBI_HOME_DIR", "")
	got, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		cfg, _ := os.UserConfigDir()
		if want := filepath.Join(cfg, "Rubi Home"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if runtime.GOOS == "darwin" && filepath.Base(filepath.Dir(got)) != "Application Support" {
		t.Errorf("a Mac keeps Home in Application Support: %q", got)
	}
	t.Setenv("RUBI_HOME_DIR", "/x/y")
	if got, _ := DefaultDir(); got != "/x/y" {
		t.Errorf("RUBI_HOME_DIR: %q", got)
	}
}

func TestInstallTargetsComeFromThePlatform(t *testing.T) {
	if DefaultHookBinary != hostpaths.HookBinary() || DefaultClaudePluginDir != hostpaths.PluginDir() {
		t.Errorf("%q %q", DefaultHookBinary, DefaultClaudePluginDir)
	}
}

// Every folder of the platform's list is refused as a session's folder, and the macOS ones only on a Mac.
func TestStartDirDenyListFollowsThePlatform(t *testing.T) {
	home := t.TempDir()
	c := &Claude{HomeDir: home}
	for _, d := range hostpaths.SensitiveHomeDirs() {
		if c.refusedStartDir(filepath.Join(home, d, "proj")) == "" {
			t.Errorf("%s under the home folder is allowed", d)
		}
	}
	lib := c.refusedStartDir(filepath.Join(home, "Library", "proj")) != ""
	if lib != (runtime.GOOS == "darwin") {
		t.Errorf("~/Library refused = %v on %s", lib, runtime.GOOS)
	}
	for _, d := range hostpaths.SensitiveSysDirs() {
		if c.refusedStartDir(filepath.Join(d, "x")) == "" {
			t.Errorf("%s is allowed", d)
		}
	}
}
