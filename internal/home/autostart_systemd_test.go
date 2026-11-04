package home

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The unit panel/install-home.sh wrote before stage P4, byte for byte (its heredoc with $bin filled in). The script now
// runs `rubi-home install-autostart`, and this must come out the same.
const oldScriptUnit = `[Unit]
Description=Rubi Home
After=network-online.target

[Service]
ExecStart=/home/ann/.rubi-home/bin/rubi-home run
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`

// What the script ran after writing the unit, in order.
var oldScriptCalls = [][]string{
	{"--user", "daemon-reload"},
	{"--user", "enable", "--now", "rubi-home.service"},
	{"--user", "restart", "rubi-home.service"},
}

func TestSystemdUnitIsWhatTheScriptWrote(t *testing.T) {
	got, err := systemdUnit("/home/ann/.rubi-home/bin/rubi-home")
	if err != nil || got != oldScriptUnit {
		t.Fatalf("err %v\n got:\n%s\nwant:\n%s", err, got, oldScriptUnit)
	}
}

func TestSystemdUnitQuotesWhatSystemdWouldSplitOrExpand(t *testing.T) {
	for in, want := range map[string]string{
		"/home/a b/.rubi-home/bin/rubi-home": `"/home/a b/.rubi-home/bin/rubi-home"`,
		`/home/a"b/rubi-home`:                `"/home/a\"b/rubi-home"`,
		"/home/100%/rubi-home":               `"/home/100%%/rubi-home"`,
		"/home/$x/rubi-home":                 `"/home/$$x/rubi-home"`,
		`/home/a\b/rubi-home`:                `"/home/a\\b/rubi-home"`,
		"/home/a;b/rubi-home":                `"/home/a;b/rubi-home"`,
	} {
		got, err := systemdUnit(in)
		if err != nil || !strings.Contains(got, "ExecStart="+want+" run\n") {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "relative/rubi-home", "/home/a\nExecStartPre=/bin/evil/rubi-home", "/home/a\x00b"} {
		if got, err := systemdUnit(bad); err == nil {
			t.Errorf("%q gave a unit: %q", bad, got)
		}
	}
}

func sysRig(t *testing.T, have ...string) (*progRun, Autostart, string) {
	t.Helper()
	home := t.TempDir()
	p := &progRun{out: map[string]string{}, err: map[string]error{}}
	if len(have) == 0 {
		have = []string{"systemctl"}
	}
	return p, systemdAutostart(AutostartEnv{Run: p.run, Home: home, LookPath: lookIn(have...)}.withDefaults()), home
}

// Call for call what the script did: the unit file in ~/.config/systemd/user, then daemon-reload, enable --now, restart.
func TestSystemdInstallDoesWhatTheScriptDid(t *testing.T) {
	p, as, home := sysRig(t)
	unit := filepath.Join(home, ".config", "systemd", "user", "rubi-home.service")
	bin := "/home/ann/.rubi-home/bin/rubi-home"
	var atFirstCall string
	p.run2 = func() {
		if atFirstCall == "" {
			b, _ := os.ReadFile(unit)
			atFirstCall = string(b)
		}
	}
	res, err := as.Install(context.Background(), AutostartSpec{Bin: bin, Logs: "/home/ann/.rubi-home/logs"})
	if err != nil || !res.Restarted {
		t.Fatalf("%+v %v", res, err)
	}
	var got [][]string
	for _, c := range p.calls {
		if c.name != "/bin/systemctl" {
			t.Fatalf("ran %s", c.name)
		}
		got = append(got, c.args)
	}
	if !reflect.DeepEqual(got, oldScriptCalls) {
		t.Fatalf("calls %q, want %q", got, oldScriptCalls)
	}
	if atFirstCall != oldScriptUnit {
		t.Errorf("when systemctl was first called the unit was:\n%s", atFirstCall)
	}
	if st, err := os.Stat(unit); err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("unit file: %v %v", st, err)
	}
	if st, _ := os.Stat(filepath.Dir(unit)); st.Mode().Perm() != 0o755 {
		t.Errorf("unit folder mode %v", st.Mode())
	}
}

func TestSystemdWithoutSystemctlChangesNothing(t *testing.T) {
	p, as, home := sysRig(t, "other")
	if _, err := as.Install(context.Background(), AutostartSpec{Bin: "/a/rubi-home"}); err == nil || !strings.Contains(err.Error(), "no systemd") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config")); err == nil || len(p.calls) != 0 {
		t.Errorf("something was written or run: %v %v", err, p.calls)
	}
	if err := as.Restart(context.Background()); err == nil {
		t.Error("restart without systemd")
	}
}

func TestSystemdReplacesALinkAtTheUnitAndReportsFailures(t *testing.T) {
	p, as, home := sysRig(t)
	unit := filepath.Join(home, ".config", "systemd", "user", "rubi-home.service")
	os.MkdirAll(filepath.Dir(unit), 0o755)
	target := filepath.Join(home, "victim")
	os.WriteFile(target, []byte("keep"), 0o644)
	if err := os.Symlink(target, unit); err != nil {
		t.Skip(err)
	}
	if _, err := as.Install(context.Background(), AutostartSpec{Bin: "/a/rubi-home"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatal("the unit was written through a link")
	}
	if li, _ := os.Lstat(unit); li.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the link stayed")
	}
	// a failing systemctl is an error that names the call and, when there is no user session, how to get one
	_, fail := exec.Command("sh", "-c", "echo 'Failed to connect to bus: No such file or directory' >&2; exit 1").Output()
	if fail == nil {
		t.Skip("no sh")
	}
	p.err["/bin/systemctl"] = fail
	_, err := as.Install(context.Background(), AutostartSpec{Bin: "/a/rubi-home"})
	if err == nil || !strings.Contains(err.Error(), "systemd (user) daemon-reload") || !strings.Contains(err.Error(), "loginctl enable-linger") {
		t.Fatalf("%v", err)
	}
	// an unusable binary path writes no file and runs nothing
	p.calls = nil
	if _, err := as.Install(context.Background(), AutostartSpec{Bin: "relative"}); !errors.Is(err, errAutostartWrite) || len(p.calls) != 0 {
		t.Errorf("%v %v", err, p.calls)
	}
}

func TestSystemdRestartRemoveLoaded(t *testing.T) {
	p, as, home := sysRig(t)
	if _, err := as.Install(context.Background(), AutostartSpec{Bin: "/a/rubi-home"}); err != nil {
		t.Fatal(err)
	}
	p.calls = nil
	if err := as.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !as.Loaded(context.Background()) {
		t.Error("not loaded")
	}
	p.err["/bin/systemctl"] = errors.New("inactive")
	if as.Loaded(context.Background()) {
		t.Error("loaded although is-active failed")
	}
	p.err["/bin/systemctl"] = nil
	if err := as.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range p.calls {
		got = append(got, strings.Join(c.args, " "))
	}
	want := []string{"--user restart rubi-home.service", "--user is-active --quiet rubi-home.service", "--user is-active --quiet rubi-home.service",
		"--user disable --now rubi-home.service", "--user daemon-reload"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", "rubi-home.service")); err == nil {
		t.Error("the unit file stayed")
	}
}
