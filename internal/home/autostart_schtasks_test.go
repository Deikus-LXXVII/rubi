package home

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

const (
	schtasks = sysDir + `\schtasks.exe`
	taskUser = `DESKTOP-1\ann`
	taskBin  = `C:\Users\ann\AppData\Local\Rubi Home\bin\rubi-home.exe`
)

func taskRig(t *testing.T) (*progRun, Autostart, string) {
	t.Helper()
	dir := t.TempDir()
	p := &progRun{out: map[string]string{}, err: map[string]error{}}
	as := schtasksAutostart(AutostartEnv{Run: p.run, SystemDir: sysDir, User: taskUser, LookPath: lookIn(schtasks)}.withDefaults())
	return p, as, filepath.Join(dir, "logs")
}

func decodeUTF16(t *testing.T, b []byte) string {
	t.Helper()
	if len(b) < 2 || b[0] != 0xff || b[1] != 0xfe || len(b)%2 != 0 {
		t.Fatalf("not UTF-16LE with a byte order mark: % x", b[:min(len(b), 8)])
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 2; i+1 < len(b); i += 2 {
		u = append(u, binary.LittleEndian.Uint16(b[i:]))
	}
	return string(utf16.Decode(u))
}

func TestTaskXMLIsValidAndLogonOnlyForTheUser(t *testing.T) {
	x := taskXML(taskBin, taskUser)
	if err := validateTaskXML(x); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<LogonTrigger>", "<UserId>" + `DESKTOP-1\ann` + "</UserId>", "<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>LeastPrivilege</RunLevel>", `<Command>"` + taskBin + `"</Command>`, "<Arguments>run</Arguments>", "<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<RestartOnFailure>", `<Actions Context="Author">`} {
		if !strings.Contains(x, want) {
			t.Errorf("the definition lacks %q", want)
		}
	}
	for _, bad := range []string{"HighestAvailable", "GroupId", "BootTrigger", "SYSTEM", "S4U", "Password", "RegistrationTrigger"} {
		if strings.Contains(x, bad) {
			t.Errorf("the definition holds %q", bad)
		}
	}
	// names and paths are escaped, not spliced
	x = taskXML(`C:\a&b\<x>\rubi-home.exe`, `DOM\a<b>&"c"`)
	if err := validateTaskXML(x); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(x, "<x>") || strings.Contains(x, `a<b>`) || !strings.Contains(x, "a&amp;b") {
		t.Errorf("not escaped:\n%s", x)
	}
}

// Whatever would make Windows ask for an administrator is refused by name.
func TestValidateTaskXMLRefusesWhatNeedsElevation(t *testing.T) {
	good := taskXML(taskBin, taskUser)
	for name, mutate := range map[string]func(string) string{
		"highest rights":   func(x string) string { return strings.Replace(x, "LeastPrivilege", "HighestAvailable", 1) },
		"a password logon": func(x string) string { return strings.Replace(x, "InteractiveToken", "Password", 1) },
		"S4U logon":        func(x string) string { return strings.Replace(x, "InteractiveToken", "S4U", 1) },
		"a group": func(x string) string {
			return strings.Replace(x, "<LogonType>", "<GroupId>S-1-5-32-544</GroupId><LogonType>", 1)
		},
		"the system account": func(x string) string { return strings.ReplaceAll(x, taskUser, `NT AUTHORITY\SYSTEM`) },
		"SYSTEM by SID":      func(x string) string { return strings.ReplaceAll(x, taskUser, "S-1-5-18") },
		"every user's logon": func(x string) string {
			return strings.Replace(x, "<UserId>"+taskUser+"</UserId>\n    </LogonTrigger>", "</LogonTrigger>", 1)
		},
		"a startup trigger": func(x string) string {
			return strings.Replace(x, "<LogonTrigger>", "<BootTrigger><Enabled>true</Enabled></BootTrigger><LogonTrigger>", 1)
		},
		"only a startup trigger": func(x string) string {
			return strings.NewReplacer("<LogonTrigger>", "<BootTrigger>", "</LogonTrigger>", "</BootTrigger>").Replace(x)
		},
		"a second trigger": func(x string) string {
			return strings.Replace(x, "</Triggers>", "<LogonTrigger><UserId>"+taskUser+"</UserId></LogonTrigger></Triggers>", 1)
		},
		"another user as principal": func(x string) string {
			return strings.Replace(x, "<Principal id=\"Author\">\n      <UserId>"+taskUser, "<Principal id=\"Author\">\n      <UserId>OTHER\\bob", 1)
		},
		"no logon type":      func(x string) string { return strings.Replace(x, "<LogonType>InteractiveToken</LogonType>", "", 1) },
		"no run level":       func(x string) string { return strings.Replace(x, "<RunLevel>LeastPrivilege</RunLevel>", "", 1) },
		"the Administrators": func(x string) string { return strings.ReplaceAll(x, taskUser, `BUILTIN\Administrators`) },
		"not xml":            func(x string) string { return "this is not a task" },
	} {
		if err := validateTaskXML(mutate(good)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// Call for call: the task is created from a UTF-16 file in the user's own folder, overwritten, stopped if it ran, and
// started. No argument asks for another account, a password or the highest rights.
func TestTaskInstallCallsAndFile(t *testing.T) {
	p, as, logs := taskRig(t)
	file := filepath.Join(filepath.Dir(logs), "rubi-home-task.xml")
	var created string
	p.run2 = func() {
		if b, err := os.ReadFile(file); err == nil && created == "" {
			created = decodeUTF16(t, b)
		}
	}
	res, err := as.Install(context.Background(), AutostartSpec{Bin: taskBin, Logs: logs})
	if err != nil || !res.Restarted || !res.WasLoaded {
		t.Fatalf("%+v %v", res, err)
	}
	var got [][]string
	for _, c := range p.calls {
		if c.name != schtasks {
			t.Fatalf("ran %q", c.name)
		}
		got = append(got, c.args)
	}
	want := [][]string{{"/Query", "/TN", "Rubi Home"}, {"/Create", "/TN", "Rubi Home", "/XML", file, "/F"}, {"/End", "/TN", "Rubi Home"}, {"/Run", "/TN", "Rubi Home"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %q\nwant  %q", got, want)
	}
	if created != taskXML(taskBin, taskUser) {
		t.Errorf("the file schtasks read:\n%s", created)
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("the task file was left behind")
	}
	for _, c := range p.calls {
		for _, a := range c.args {
			switch strings.ToUpper(a) {
			case "/RU", "/RP", "/RL", "/SC", "/IT", "/S", "/U", "/P", "HIGHEST", "SYSTEM", "RUNAS":
				t.Errorf("an argument asks for more: %v", c.args)
			}
		}
	}
}

func TestTaskInstallRefusesToElevate(t *testing.T) {
	_, denied := exec.Command("sh", "-c", "echo 'ERROR: Access is denied.' >&2; exit 1").Output() // Output keeps the error text
	if denied == nil {
		t.Skip("no sh")
	}
	p, as, logs := taskRig(t)
	p.err[schtasks] = denied
	_, err := as.Install(context.Background(), AutostartSpec{Bin: taskBin, Logs: logs})
	if !errors.Is(err, ErrNeedsElevation) {
		t.Fatalf("%v", err)
	}
	// it stopped at the first refusal: it did not go on, and it did not try anything else
	if len(p.calls) != 2 { // the query (whose failure only means "not there"), then the create
		t.Fatalf("calls after a refusal: %+v", p.calls)
	}
	for _, c := range p.calls {
		if c.name != schtasks {
			t.Errorf("ran %q", c.name)
		}
	}
	// other failures are errors, not elevation
	p, as, logs = taskRig(t)
	p.err[schtasks] = exitErr(t, 1)
	if _, err := as.Install(context.Background(), AutostartSpec{Bin: taskBin, Logs: logs}); err == nil || errors.Is(err, ErrNeedsElevation) {
		t.Errorf("an ordinary failure: %v", err)
	}
}

func TestTaskInstallRefusesAccountsAndPathsThatCouldNotBeTheUser(t *testing.T) {
	for _, user := range []string{"", "SYSTEM", `NT AUTHORITY\SYSTEM`, "S-1-5-18", `BUILTIN\Administrators`} {
		p := &progRun{out: map[string]string{}, err: map[string]error{}}
		as := schtasksAutostart(AutostartEnv{Run: p.run, SystemDir: sysDir, User: user, LookPath: lookIn(schtasks)}.withDefaults())
		_, err := as.Install(context.Background(), AutostartSpec{Bin: taskBin, Logs: `C:\x\logs`})
		if err == nil || len(p.calls) != 0 {
			t.Errorf("user %q: %v, calls %v", user, err, p.calls)
		}
		if user != "" && !errors.Is(err, ErrNeedsElevation) {
			t.Errorf("user %q: %v", user, err)
		}
	}
	p, as, logs := taskRig(t)
	for _, bin := range []string{"relative.exe", "", "C:\\a\"b\\rubi-home.exe", "C:\\a\nb\\rubi-home.exe"} {
		if _, err := as.Install(context.Background(), AutostartSpec{Bin: bin, Logs: logs}); err == nil {
			t.Errorf("%q was accepted", bin)
		}
	}
	if len(p.calls) != 0 {
		t.Errorf("ran for a bad path: %v", p.calls)
	}
}

func TestTaskNeedsTheSystemsOwnSchtasks(t *testing.T) {
	p := &progRun{}
	if _, err := schtasksAutostart(AutostartEnv{Run: p.run, User: taskUser, LookPath: lookIn("schtasks.exe")}.withDefaults()).
		Install(context.Background(), AutostartSpec{Bin: taskBin, Logs: `C:\x\logs`}); err == nil || len(p.calls) != 0 {
		t.Errorf("with no system folder: %v %v", err, p.calls)
	}
	as := schtasksAutostart(AutostartEnv{Run: p.run, SystemDir: sysDir, User: taskUser, LookPath: lookIn("schtasks.exe")}.withDefaults())
	if _, err := as.Install(context.Background(), AutostartSpec{Bin: taskBin, Logs: `C:\x\logs`}); err == nil || len(p.calls) != 0 {
		t.Errorf("schtasks from PATH was used: %v %v", err, p.calls)
	}
}

func TestTaskRestartRemoveLoaded(t *testing.T) {
	p, as, _ := taskRig(t)
	if err := as.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !as.Loaded(context.Background()) {
		t.Error("not loaded")
	}
	if err := as.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range p.calls {
		got = append(got, strings.Join(c.args, " "))
	}
	want := []string{"/End /TN Rubi Home", "/Run /TN Rubi Home", "/Query /TN Rubi Home", "/End /TN Rubi Home", "/Delete /TN Rubi Home /F"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	p.err[schtasks] = errors.New("not there")
	if as.Loaded(context.Background()) {
		t.Error("loaded with no task")
	}
}

func TestUTF16LE(t *testing.T) {
	if got := decodeUTF16(t, utf16LE("a\u00e9\U0001F600")); got != "a\u00e9\U0001F600" {
		t.Fatalf("%q", got)
	}
}

// Removing a task that is not there succeeds (the installer's uninstall may run after an upgrade already removed it);
// a task that is there but could not be deleted, or a denied delete, is still an error.
func TestTaskRemoveOfAMissingTaskIsDone(t *testing.T) {
	p, as, _ := taskRig(t)
	p.err[schtasks] = errors.New("exit status 1")
	if err := as.Remove(context.Background()); err != nil {
		t.Errorf("a missing task: %v", err)
	}

	p, as, _ = taskRig(t)
	p.run2 = func() {
		if p.calls[len(p.calls)-1].args[0] == "/Delete" {
			p.err[schtasks] = errors.New("exit status 1")
		} else {
			delete(p.err, schtasks)
		}
	}
	if err := as.Remove(context.Background()); err == nil {
		t.Error("a task that is still there was reported removed")
	}

	p, as, _ = taskRig(t)
	p.out[schtasks] = "ERROR: Access is denied."
	p.err[schtasks] = errors.New("exit status 1")
	if err := as.Remove(context.Background()); !errors.Is(err, ErrNeedsElevation) {
		t.Errorf("a denied delete: %v", err)
	}
}
