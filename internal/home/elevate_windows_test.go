package home

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
)

// The elevated script itself, run on a real Windows (the CI runner is an administrator, so UAC does not ask): it installs
// into a temporary folder, refuses a plan whose file changed, a binary that is not the approved one and a plugin file that
// is not, and leaves what it installs readable by Users and changeable only by Administrators and SYSTEM, which the same
// check InstallHook ends with (fsx.TrustedByAdmin) confirms.

func isElevated(t *testing.T) bool {
	t.Helper()
	out, err := exec.Command("whoami", "/groups").Output()
	return err == nil && strings.Contains(string(out), "S-1-16-12288")
}

// runInner runs the elevated command the way UAC would start it (the bootstrap, checking the install script's file against
// its hash), without the prompt. tamper, when set, changes the script file after its hash was taken.
func runInner(t *testing.T, planPath, hash string, tamper ...func(path string)) int {
	t.Helper()
	inner, err := uacInner(planPath, hash)
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(filepath.Dir(planPath), "install.ps1")
	if err := os.WriteFile(scriptPath, []byte(inner), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(inner))
	boot, err := uacBoot(scriptPath, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range tamper {
		f(scriptPath)
	}
	ps := os.Getenv("SystemRoot") + `\System32\WindowsPowerShell\v1.0\powershell.exe`
	out, err := exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", psEncoded(boot)).CombinedOutput()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if len(out) > 0 {
			t.Logf("exit %d: %s", ee.ExitCode(), out) // the script's own words, for a refusal that is not the expected one
		}
		return ee.ExitCode()
	}
	t.Fatal(err)
	return -1
}

func TestWindowsElevatedScriptInstallsAndRefuses(t *testing.T) {
	if !isElevated(t) {
		t.Skip("needs an administrator to set the owner")
	}
	root := t.TempDir()
	src := filepath.Join(root, "src", "rubi-home.exe")
	os.MkdirAll(filepath.Dir(src), 0o755)
	os.WriteFile(src, []byte("MZ binary one"), 0o755)
	sum, _ := fileSHA256(src)
	dst := filepath.Join(root, "Program Files", "Rubi Home", "rubi-home.exe")
	pdst := filepath.Join(root, "Program Files", "Rubi Home", "claude-plugin")
	files := []PluginFile{{Path: "rubi-sisters/.mcp.json", Data: []byte(`{"a":1}`), SHA256: sha256Hex([]byte(`{"a":1}`))}}
	mk := func(files []PluginFile) (InstallPlan, string) {
		p := InstallPlan{Windows: true, Src: src, Dst: dst, SHA256: sum, Script: windowsInstallScript(src, dst, sum)}
		p.Plugin = &PluginPlan{Dst: pdst, Files: files, Script: windowsPluginScript(pdst, files)}
		planPath := filepath.Join(root, "plan.json")
		if err := os.WriteFile(planPath, []byte(p.FullScript()), 0o600); err != nil {
			t.Fatal(err)
		}
		return p, planPath
	}

	// a plan file that is not the one with the hash on the command line
	p, planPath := mk(files)
	if code := runInner(t, planPath, strings.Repeat("0", 64)); code != uacPlanChanged {
		t.Fatalf("a plan with another hash: exit %d", code)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("installed after a plan mismatch")
	}
	// the install script swapped after its hash was taken: the bootstrap refuses it
	if code := runInner(t, planPath, PlanSHA256(p), func(path string) { os.WriteFile(path, []byte("exit 0"), 0o600) }); code != uacPlanChanged {
		t.Fatalf("a swapped install script: exit %d", code)
	}
	// the binary swapped after the approval
	os.WriteFile(src, []byte("MZ evil"), 0o755)
	if code := runInner(t, planPath, PlanSHA256(p)); code != uacFileChanged {
		t.Fatalf("a swapped binary: exit %d", code)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("installed a swapped binary")
	}
	os.WriteFile(src, []byte("MZ binary one"), 0o755)
	// a plugin file that is not the approved one
	bad := []PluginFile{{Path: files[0].Path, Data: files[0].Data, SHA256: strings.Repeat("a", 64)}}
	p2, planPath2 := mk(bad)
	if code := runInner(t, planPath2, PlanSHA256(p2)); code != uacPluginFiles {
		t.Fatalf("a wrong plugin file: exit %d", code)
	}
	if _, err := os.Stat(pdst); err == nil {
		t.Fatal("a plugin was installed from a wrong file")
	}
	// a plugin folder that is not called claude-plugin, or that exists and is not the Rubi plugin
	other := filepath.Join(root, "Program Files", "Rubi Home", "somewhere-else")
	pBad := InstallPlan{Windows: true, Src: src, Dst: dst, SHA256: sum, Script: windowsInstallScript(src, dst, sum),
		Plugin: &PluginPlan{Dst: other, Files: files, Script: windowsPluginScript(other, files)}}
	planBad := filepath.Join(root, "plan-bad.json")
	os.WriteFile(planBad, []byte(pBad.FullScript()), 0o600)
	if code := runInner(t, planBad, PlanSHA256(pBad)); code != uacNotRubi {
		t.Fatalf("a plugin folder with another name: exit %d", code)
	}
	foreign := filepath.Join(root, "Program Files", "Rubi Home", "claude-plugin-foreign", "claude-plugin")
	os.MkdirAll(foreign, 0o755)
	os.WriteFile(filepath.Join(foreign, "precious.txt"), []byte("keep"), 0o644)
	pF := InstallPlan{Windows: true, Src: src, Dst: dst, SHA256: sum, Script: windowsInstallScript(src, dst, sum),
		Plugin: &PluginPlan{Dst: foreign, Files: files, Script: windowsPluginScript(foreign, files)}}
	os.WriteFile(planBad, []byte(pF.FullScript()), 0o600)
	if code := runInner(t, planBad, PlanSHA256(pF)); code != uacNotRubi {
		t.Fatalf("a folder that is not the Rubi plugin: exit %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(foreign, "precious.txt")); string(b) != "keep" {
		t.Fatal("a foreign folder was touched")
	}
	// a target that is not called rubi-home.exe
	odd := filepath.Join(root, "Program Files", "Rubi Home", "other.exe")
	pO := InstallPlan{Windows: true, Src: src, Dst: odd, SHA256: sum, Script: windowsInstallScript(src, odd, sum)}
	os.WriteFile(planBad, []byte(pO.FullScript()), 0o600)
	if code := runInner(t, planBad, PlanSHA256(pO)); code != uacNotRubi {
		t.Fatalf("a target with another name: exit %d", code)
	}
	if _, err := os.Stat(odd); err == nil {
		t.Fatal("wrote a file that is not rubi-home.exe")
	}
	// the good plan
	p, planPath = mk(files)
	if code := runInner(t, planPath, PlanSHA256(p)); code != 0 {
		t.Fatalf("the good plan: exit %d", code)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "MZ binary one" {
		t.Fatalf("installed: %q %v", b, err)
	}
	for _, path := range []string{dst, pdst, filepath.Join(pdst, "rubi-sisters", ".mcp.json")} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if ok, why := fsx.TrustedByAdmin(path, fi); !ok {
			t.Errorf("%s is not admin-only: %s", path, why)
		}
	}
}
