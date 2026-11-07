package repocheck

// panel/install-home.sh and the Linux start at login (docs/plans/home-platforms.md, stage P4). The script no longer writes
// the systemd unit: it runs `rubi-home install-autostart`, which makes the same unit and runs the same systemctl calls
// (internal/home/autostart_systemd_test.go holds the old unit and the old calls byte for byte). These tests keep the script
// to that: no unit of its own, the call, the same message, the same behaviour when the call fails, and the macOS part
// untouched.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// linuxBranch is the script's `else` branch of the autostart: what runs where `uname` is not Darwin.
func linuxBranch(t *testing.T) string {
	t.Helper()
	sh := readRepo(t, "panel/install-home.sh")
	_, afterIf, ok := strings.Cut(sh, `if [ "$os" = darwin ]; then`)
	if !ok {
		t.Fatal("no darwin branch in install-home.sh")
	}
	_, rest, ok := strings.Cut(afterIf, "\nelse\n")
	if !ok {
		t.Fatal("no else branch in install-home.sh")
	}
	body, _, ok := strings.Cut(rest, "\nfi\n")
	if !ok {
		t.Fatal("else branch not closed")
	}
	return body
}

func TestInstallHomeShDoesNotWriteTheSystemdUnitItself(t *testing.T) {
	sh := readRepo(t, "panel/install-home.sh")
	for _, bad := range []string{"<<UNIT", "[Service]", "ExecStart=", "systemctl", ".config/systemd"} {
		if strings.Contains(codeOf(sh), bad) {
			t.Errorf("install-home.sh still has %q", bad)
		}
	}
	if b := linuxBranch(t); !strings.Contains(b, `"$bin" install-autostart --bin "$bin" || fail "couldn't set Rubi Home up to start at login"`) ||
		!strings.Contains(b, `say "running as a systemd user service (for it to run without you logged in: loginctl enable-linger)"`) {
		t.Errorf("the Linux branch:\n%s", b)
	}
}

// codeOf drops the comment lines of a shell script.
func codeOf(sh string) string {
	var out []string
	for _, l := range strings.Split(sh, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// The branch is run with a stand-in for rubi-home: it gets exactly `install-autostart --bin <bin>`, the script prints its
// message after it, and when the call fails the script stops with its own error (as it did when systemctl failed).
func TestInstallHomeShLinuxBranchRuns(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "rubi home", "rubi-home") // a space in the path
	os.MkdirAll(filepath.Dir(bin), 0o755)
	stub := func(status string) {
		os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s|' \"$@\" >> '"+log+"'\nexit "+status+"\n"), 0o755)
	}
	harness := `set -eu
say() { printf 'rubi-home: %s\n' "$*"; }
fail() { printf 'rubi-home: error: %s\n' "$*" >&2; exit 1; }
bin=$1
` + linuxBranch(t) + `
echo after
`
	script := filepath.Join(dir, "branch.sh")
	os.WriteFile(script, []byte(harness), 0o644)

	stub("0")
	out, err := exec.Command("sh", script, bin).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, _ := os.ReadFile(log); string(got) != "install-autostart|--bin|"+bin+"|" {
		t.Errorf("the call was %q", got)
	}
	if want := "rubi-home: running as a systemd user service (for it to run without you logged in: loginctl enable-linger)\nafter\n"; string(out) != want {
		t.Errorf("output %q, want %q", out, want)
	}

	os.Remove(log)
	stub("3")
	out, err = exec.Command("sh", script, bin).CombinedOutput()
	if err == nil || strings.Contains(string(out), "after") || !strings.Contains(string(out), "couldn't set Rubi Home up to start at login") ||
		strings.Contains(string(out), "running as a systemd") {
		t.Errorf("a failing install-autostart: %v\n%s", err, out)
	}
}

// Already paired: the installer must not mint another code. Unpaired: it still prints one.
func TestInstallHomeShSkipsPairingWhenAlreadyPaired(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	sh := readRepo(t, "panel/install-home.sh")
	i := strings.Index(sh, "# pair-or-skip: begin\n")
	j := strings.Index(sh, "# pair-or-skip: end\n")
	if i < 0 || j < i {
		t.Fatal("the pairing decision is not marked in install-home.sh")
	}
	body := sh[i+len("# pair-or-skip: begin\n") : j]
	dir := t.TempDir()
	bin := filepath.Join(dir, "rubi-home")
	script := filepath.Join(dir, "pair.sh")
	write := func(status string) {
		os.WriteFile(bin, []byte("#!/bin/sh\ncase \"$1\" in\nstatus) printf '%s\\n' \""+status+"\" ;;\npair) echo MINTED ;;\n*) echo \"unexpected $*\" >&2; exit 9 ;;\nesac\n"), 0o755)
	}
	os.WriteFile(script, []byte("set -eu\nsay() { printf 'rubi-home: %s\\n' \"$*\"; }\nbin=$1\n"+body), 0o644)

	write("Paired with 2 Rubi")
	out, err := exec.Command("sh", script, bin).CombinedOutput()
	if err != nil {
		t.Fatalf("paired: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "MINTED") || !strings.Contains(string(out), "already paired") {
		t.Fatalf("minted a code while paired:\n%s", out)
	}
	write("Paired with 0 Rubi")
	out, err = exec.Command("sh", script, bin).CombinedOutput()
	if err != nil {
		t.Fatalf("unpaired: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "MINTED") || strings.Contains(string(out), "already paired") {
		t.Fatalf("did not mint a code when nothing is paired:\n%s", out)
	}
}

// The macOS part of the script is what it was: the LaunchAgent heredoc (which internal/home keeps equal to AgentPlist on a
// Mac) and the launchctl calls, in order.
func TestInstallHomeShMacPartIsUntouched(t *testing.T) {
	sh := readRepo(t, "panel/install-home.sh")
	i := strings.Index(sh, `if [ "$os" = darwin ]; then`)
	j := strings.Index(sh, "\nelse\n  # The systemd user unit")
	if i < 0 || j < i {
		t.Fatal("the macOS part is not where it was")
	}
	mac := sh[i:j]
	for _, want := range []string{`plist="$HOME/Library/LaunchAgents/com.rubi-project.home.plist"`, "<<PLIST", "<key>RunAtLoad</key><true/>",
		`launchctl bootout "gui/$(id -u)" "$plist" 2>/dev/null || true`, `launchctl bootstrap "gui/$(id -u)" "$plist"`,
		`say "running at login (LaunchAgent com.rubi-project.home)"`} {
		if !strings.Contains(mac, want) {
			t.Errorf("the macOS part lacks %q", want)
		}
	}
	if strings.Contains(mac, "install-autostart") {
		t.Error("the macOS part was changed to use install-autostart")
	}
}

// The command Rubi Home hands the Bot on a computer with no polkit agent runs the plan's script with sh, by absolute paths. It
// never runs Rubi Home's own binary as root (a file the user can write), and install-hook has no flag that would let a root
// process take a plan from outside.
func TestHeadlessCommandNeverRunsRubiHomeAsRoot(t *testing.T) {
	state := readRepo(t, "internal/home/hookstate.go")
	i := strings.Index(state, "func sudoCommand")
	if i < 0 {
		t.Fatal("no sudoCommand")
	}
	end := strings.Index(state[i:], "\n}\n")
	body := state[i : i+end]
	if !strings.Contains(body, `[]string{"/usr/bin/sudo", "--", "/bin/sh", "-c", p.FullScript()}`) {
		t.Errorf("the headless command is not /usr/bin/sudo -- /bin/sh -c SCRIPT:\n%s", body)
	}
	for _, bad := range []string{".Src", "install-hook", "rubi-home", "Executable"} {
		if strings.Contains(body, bad) {
			t.Errorf("the headless command mentions %q", bad)
		}
	}
	main := readRepo(t, "cmd/rubi-home/main.go")
	for _, bad := range []string{"expect-plan-sha256", "ExpectPlanSHA256"} {
		if strings.Contains(main, bad) {
			t.Errorf("install-hook has %q: a root process must not take a plan from outside", bad)
		}
	}
	// no form of Elevator runs Rubi Home as root
	for _, f := range []string{"elevate.go", "elevate_pkexec.go", "elevate_uac.go", "elevate_darwin.go"} {
		src := readRepo(t, "internal/home/"+f)
		if strings.Contains(src, "rootElevator") || strings.Contains(src, "RootDirect") {
			t.Errorf("%s has a root form", f)
		}
	}
}
