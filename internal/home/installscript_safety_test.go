package home

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	claudeplugin "github.com/Deikus-LXXVII/rubi/claude-plugin"
)

// What the root script and the root CLI will and will not replace, whatever they are told (stage P5 review, C2). The scripts
// are run here by sh as an ordinary user: every refusal comes before anything is changed.

func scriptRig(t *testing.T) (src, want string, tools installTools) {
	t.Helper()
	tools = hostTools(t)
	src, _ = installFixture(t)
	want, _ = fileSHA256(src)
	return src, want, tools
}

func TestInstallScriptReplacesOnlyAFileCalledRubiHome(t *testing.T) {
	src, want, tools := scriptRig(t)
	dir := t.TempDir()
	victim := filepath.Join(dir, "sudo")
	os.WriteFile(victim, []byte("the system's"), 0o755)
	for _, dst := range []string{victim, filepath.Join(dir, "rubi-home.sh"), filepath.Join(dir, "x", "rubi-home-old"), filepath.Join(dir, "rubi-home", "other")} {
		out, err := runScript(t, installScript(tools, src, dst, want, false))
		if err == nil || !strings.Contains(out, "is not a rubi-home file") {
			t.Errorf("%s: %v %s", dst, err, out)
		}
	}
	if b, _ := os.ReadFile(victim); string(b) != "the system's" {
		t.Fatal("a file that is not rubi-home was replaced")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*")); len(m) != 1 {
		t.Fatalf("something was made: %v", m)
	}
	// the right name works
	dst := filepath.Join(dir, "bin", "rubi-home")
	if out, err := runScript(t, installScript(tools, src, dst, want, false)); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}

// One installation at a time: a fresh lock stops a second run, a stale one (an earlier run was cut off) does not, and the lock
// is gone when the script ends, however it ends.
func TestInstallScriptLock(t *testing.T) {
	src, want, tools := scriptRig(t)
	dir := t.TempDir()
	dst := filepath.Join(dir, "rubi-home")
	lock := filepath.Join(dir, ".rubi-home.lock")
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runScript(t, installScript(tools, src, dst, want, false))
	if err == nil || !strings.Contains(out, "another installation of the hook is running") {
		t.Fatalf("a fresh lock: %v %s", err, out)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("installed under a held lock")
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("the other run's lock was removed")
	}
	old := time.Now().Add(-20 * time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if out, err := runScript(t, installScript(tools, src, dst, want, false)); err != nil {
		t.Fatalf("a stale lock: %v %s", err, out)
	}
	if _, err := os.Stat(lock); err == nil {
		t.Error("the lock stayed after a successful run")
	}
	// a failing run (a wrong hash) lets go of the lock as well
	if out, err := runScript(t, installScript(tools, src, dst, strings.Repeat("0", 64), false)); err == nil || !strings.Contains(out, "nothing installed") {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := os.Stat(lock); err == nil {
		t.Error("the lock stayed after a failed run")
	}
}

func TestPluginScriptReplacesOnlyTheRubiPluginFolder(t *testing.T) {
	tools := hostTools(t)
	if b64, err := exec.LookPath("base64"); err != nil {
		t.Skip("no base64")
	} else if runtime.GOOS == "darwin" {
		tools.B64D = b64 + " -D"
	} else {
		tools.B64D = b64 + " -d"
	}
	files, err := EmbeddedPluginFiles()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	// a folder with another name (/usr/bin, say): refused before anything moves
	usrBin := filepath.Join(base, "usr", "bin")
	os.MkdirAll(usrBin, 0o755)
	os.WriteFile(filepath.Join(usrBin, "sudo"), []byte("x"), 0o755)
	out, err := runScript(t, pluginScript(tools, usrBin, files, false))
	if err == nil || !strings.Contains(out, "is not a claude-plugin folder") {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(usrBin, "sudo")); err != nil {
		t.Fatal("the folder was moved or emptied")
	}
	if m, _ := filepath.Glob(filepath.Join(base, "usr", ".claude-plugin.*")); len(m) != 0 {
		t.Fatalf("temporary entries: %v", m)
	}
	// a folder with the right name that is not the Rubi plugin: refused, untouched
	foreign := filepath.Join(base, "other", "claude-plugin")
	os.MkdirAll(foreign, 0o755)
	os.WriteFile(filepath.Join(foreign, "precious.txt"), []byte("keep"), 0o644)
	out, err = runScript(t, pluginScript(tools, foreign, files, false))
	if err == nil || !strings.Contains(out, "is not a Rubi plugin folder") {
		t.Fatalf("%v %s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(foreign, "precious.txt")); string(b) != "keep" {
		t.Fatal("a foreign folder was replaced")
	}
	// the folder an earlier install left (it has the marketplace's manifest) is replaced
	mine := filepath.Join(base, "mine", "claude-plugin")
	os.MkdirAll(filepath.Join(mine, ".claude-plugin"), 0o755)
	os.WriteFile(filepath.Join(mine, ".claude-plugin", "marketplace.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(mine, "old-file"), []byte("old"), 0o644)
	if out, err := runScript(t, pluginScript(tools, mine, files, false)); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(mine, "old-file")); err == nil {
		t.Error("the old plugin's file stayed")
	}
	// and so is none at all
	fresh := filepath.Join(base, "fresh", "claude-plugin")
	if out, err := runScript(t, pluginScript(tools, fresh, files, false)); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	// the planner says the same before any script is made
	for _, dst := range []string{usrBin, "/usr/bin", "/etc"} {
		if _, err := planClaudePlugin(os.DirFS("."), dst, false, tools); err == nil {
			t.Errorf("%s was planned", dst)
		}
	}
	if _, err := planClaudePlugin(claudeplugin.Files, foreign, false, tools); err == nil || !strings.Contains(err.Error(), "not a Rubi plugin folder") {
		t.Errorf("a foreign claude-plugin folder was planned: %v", err)
	}
}

// As root the CLI installs to the two fixed places only.
func TestRootInstallsToTheFixedPlacesOnly(t *testing.T) {
	old := geteuid
	t.Cleanup(func() { geteuid = old })
	geteuid = func() int { return 0 }
	for name, o := range map[string]InstallOptions{
		"another hook target":     {Dst: "/usr/bin/sudo"},
		"a hook next to the real": {Dst: DefaultHookBinary + "2"},
		"a plugin in /usr/bin":    {Dst: DefaultHookBinary, WithPlugin: true, PluginDst: "/usr/bin"},
		"a plugin elsewhere":      {Dst: DefaultHookBinary, WithPlugin: true, PluginDst: "/opt/claude-plugin"},
	} {
		if err := checkRootTargets(o); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for name, o := range map[string]InstallOptions{
		"the defaults":                             {Dst: DefaultHookBinary},
		"the defaults with plugin":                 {Dst: DefaultHookBinary, WithPlugin: true},
		"the plugin named by default":              {Dst: DefaultHookBinary, WithPlugin: true, PluginDst: DefaultClaudePluginDir},
		"a plugin path ignored without the plugin": {Dst: DefaultHookBinary, PluginDst: "/usr/bin"},
	} {
		if err := checkRootTargets(o); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// InstallHook itself refuses, even for a dry run
	src, _ := installFixture(t)
	if err := InstallHook(context.Background(), &strings.Builder{}, InstallOptions{Src: src, Dst: "/usr/bin/sudo", DryRun: true}); codeOf(err) != CodeBadPath {
		t.Errorf("InstallHook as root to /usr/bin/sudo: %v", err)
	}
	geteuid = func() int { return 1000 }
	if err := checkRootTargets(InstallOptions{Dst: "/anything/rubi-home"}); err != nil {
		t.Errorf("a user may choose (the administrator step then asks): %v", err)
	}
}
