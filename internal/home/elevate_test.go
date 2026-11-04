package home

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
)

// ---- Linux: pkexec ----

type elevCall struct {
	env  []string
	name string
	args []string
}

type elevRun struct {
	calls []elevCall
	out   string
	err   error
	check func()
}

func (e *elevRun) exec(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	e.calls = append(e.calls, elevCall{env, name, args})
	if e.check != nil {
		e.check()
	}
	return []byte(e.out), e.err
}

func pkexecRig(e *elevRun, have []string, env ...string) Elevator {
	return pkexecElevator(ElevateEnv{Exec: e.exec, LookPath: lookIn(have...), Getenv: envOf(env...), Geteuid: func() int { return 1000 }}.withDefaults())
}

func TestPkexecIsArgvOnlyAndTheScriptIsOneArgument(t *testing.T) {
	hostile := "echo 'x'; $(id) \"q\" `tick` \n rm -rf /"
	p := InstallPlan{Script: hostile}
	e := &elevRun{}
	if err := pkexecRig(e, []string{"/usr/bin/pkexec"}, "DISPLAY", ":0").Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	want := []elevCall{{[]string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}, "/usr/bin/pkexec", []string{"/bin/sh", "-c", p.FullScript()}}}
	if !reflect.DeepEqual(e.calls, want) {
		t.Fatalf("%+v", e.calls)
	}
	// nothing but the plan's script reaches the program, as one argument; no shell line is built around it
	for i, a := range e.calls[0].args {
		if i != 2 && (strings.Contains(a, "echo") || strings.Contains(a, "rm -rf")) {
			t.Errorf("argument %d holds script text: %q", i, a)
		}
	}
}

func TestPkexecAvailability(t *testing.T) {
	ctx := context.Background()
	const px = "/usr/bin/pkexec"
	for name, c := range map[string]struct {
		have []string
		env  []string
		ok   bool
	}{
		"X11":                  {[]string{px}, []string{"DISPLAY", ":0"}, true},
		"Wayland":              {[]string{px}, []string{"WAYLAND_DISPLAY", "w"}, true},
		"a logind x11 session": {[]string{px}, []string{"XDG_SESSION_TYPE", "x11"}, true},
		"a logind wayland":     {[]string{px}, []string{"XDG_SESSION_TYPE", "Wayland"}, true},
		"a text session":       {[]string{px}, []string{"XDG_SESSION_TYPE", "tty"}, false},
		"ssh, no desktop":      {[]string{px}, []string{"XDG_SESSION_TYPE", "unspecified"}, false},
		"no desktop":           {[]string{px}, nil, false},
		"no pkexec":            {[]string{"/usr/bin/sudo"}, []string{"DISPLAY", ":0"}, false},
		"a pkexec elsewhere":   {[]string{"pkexec", "/tmp/pkexec"}, []string{"DISPLAY", ":0"}, false}, // only /usr/bin/pkexec counts
		"neither":              {nil, nil, false},
	} {
		err := pkexecRig(&elevRun{}, c.have, c.env...).Available(ctx)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrNoAgent)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// root needs no prompt
	e := pkexecElevator(ElevateEnv{LookPath: lookIn(), Getenv: envOf(), Geteuid: func() int { return 0 }}.withDefaults())
	if e.Available(ctx) != nil {
		t.Error("root was refused")
	}
	// without pkexec Run does not try anything
	r := &elevRun{}
	if err := pkexecRig(r, nil).Run(ctx, InstallPlan{}); !errors.Is(err, ErrNoAgent) || len(r.calls) != 0 {
		t.Errorf("%v %v", err, r.calls)
	}
}

// The words pkexec prints decide, not the exit status (the script's own failures end in 126 or 127 too), and only the
// lines pkexec writes itself count: the script's output is in the same stream and must not be able to say "dismissed".
func TestPkexecResultMapping(t *testing.T) {
	_, e1 := exec.Command("sh", "-c", "exit 126").Output()
	_, e127 := exec.Command("sh", "-c", "exit 127").Output()
	if e1 == nil || e127 == nil {
		t.Skip("no sh")
	}
	const pk = "Error executing command as another user: "
	for name, c := range map[string]struct {
		out    string
		err    error
		cancel bool
		agent  bool
	}{
		"dialog dismissed":     {pk + "Request dismissed\n", e1, true, false},
		"not authorized":       {pk + "Not authorized\n\nThis incident has been reported.\n", e127, true, false},
		"no agent":             {pk + "No authentication agent found.\n", e127, false, true},
		"authority error":      {"Error getting authority: Error initializing authority: ... No authentication agent found\n", e127, false, true},
		"the script exits 127": {"/bin/sh: 3: nosuchtool: not found\n", e127, false, false},
		"the script exits 126": {"/bin/sh: 1: x: Permission denied\n", e1, false, false},
		// the script's own output saying the words, anywhere but at the start of a line of pkexec's
		"script says dismissed":      {"cp: cannot stat 'Request dismissed': No such file\n", e1, false, false},
		"script says Not authorized": {"echo: the file said Not authorized\n", e127, false, false},
		"script quotes pkexec":       {"cat: " + pk + "Request dismissed: No such file\n", e127, false, false},
		"script says no agent":       {"  " + pk + "No authentication agent found.\n", e127, false, false},
	} {
		err := pkexecResult([]byte(c.out), c.err)
		if errors.Is(err, ErrAdminCancelled) != c.cancel || errors.Is(err, ErrNoAgent) != c.agent || (err == nil) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// ---- Windows: UAC ----

func decodePS(t *testing.T, enc string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(b)%2 != 0 {
		t.Fatalf("not an encoded command: %v", err)
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

func winPlan(t *testing.T, dst string) InstallPlan {
	t.Helper()
	files := []PluginFile{{Path: "a/b.json", Data: []byte("{}"), SHA256: "aa"}}
	return InstallPlan{Windows: true, Src: `C:\Users\ann\AppData\Local\Rubi Home\bin\rubi-home.exe`, Dst: dst, SHA256: strings.Repeat("c", 64),
		Script: windowsInstallScript(`C:\Users\ann\AppData\Local\Rubi Home\bin\rubi-home.exe`, dst, strings.Repeat("c", 64)),
		Plugin: &PluginPlan{Dst: `C:\Program Files\Rubi Home\claude-plugin`, Files: files, Script: windowsPluginScript(`C:\Program Files\Rubi Home\claude-plugin`, files)}}
}

const uacSys = `C:\Windows\System32`

func uacRig(e *elevRun, dir string) Elevator {
	return uacElevator(ElevateEnv{Exec: e.exec, SystemDir: uacSys, LookPath: lookIn(uacSys + `\WindowsPowerShell\v1.0\powershell.exe`),
		Getenv: envOf("USERPROFILE", `C:\Users\ann`, "LOCALAPPDATA", `C:\Users\ann\AppData\Local`, "TEMP", `C:\Users\ann\AppData\Local\Temp`,
			"PATH", `C:\evil`, "PSModulePath", `C:\evil\modules`),
		TempDir: func() (string, error) { return dir, nil }}.withDefaults())
}

func regexpFind(t *testing.T, s, re string) string {
	t.Helper()
	m := regexp.MustCompile(re).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no match for %s in:\n%s", re, s)
	}
	return m[1]
}

// What the elevated process is given: a constant bootstrap with the hash of the install script and its path in base64; the
// install script, which has the hash of the plan and its path in base64; the plan. The test follows the chain from the command
// line and checks each link against the file it names, while the files exist.
func TestUACChainOfHashes(t *testing.T) {
	dir := t.TempDir()
	p := winPlan(t, `C:\Program Files\Rubi Home\rubi-home.exe`)
	e := &elevRun{}
	planFile, scriptFile := filepath.Join(dir, "plan.json"), filepath.Join(dir, "install.ps1")
	var script, plan []byte
	e.check = func() {
		script, _ = os.ReadFile(scriptFile)
		plan, _ = os.ReadFile(planFile)
		if st, _ := os.Stat(planFile); st != nil && st.Mode().Perm()&0o077 != 0 && !strings.Contains(strings.ToLower(os.Getenv("OS")), "windows") {
			t.Errorf("plan file mode %v", st.Mode())
		}
	}
	if err := uacRig(e, dir).Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	ps := uacSys + `\WindowsPowerShell\v1.0\powershell.exe`
	if len(e.calls) != 1 || e.calls[0].name != ps {
		t.Fatalf("%+v", e.calls)
	}
	a := e.calls[0].args
	if len(a) != 6 || !reflect.DeepEqual(a[:5], []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand"}) || !base64Text.MatchString(a[5]) {
		t.Fatalf("argv %q", a)
	}
	// the command line of the relay: base64 and constants; no path, name or hash in clear text
	for _, bad := range []string{`C:\`, "Rubi", "Program Files", p.SHA256, PlanSHA256(p)} {
		if strings.Contains(strings.Join(a, " "), bad) {
			t.Errorf("the command line holds %q", bad)
		}
	}
	launch := decodePS(t, a[5])
	bootB64 := regexpFind(t, launch, `'-EncodedCommand', '([A-Za-z0-9+/=]+)'`)
	if !strings.Contains(launch, "-Verb RunAs") || !strings.Contains(launch, ps) {
		t.Errorf("the launcher:\n%s", launch)
	}
	boot := decodePS(t, bootB64)
	// link 1: the bootstrap's hash is the hash of the install script file, and its path is that file
	file := regexpFind(t, boot, `FromBase64String\('([A-Za-z0-9+/=]+)'\)`)
	hash := regexpFind(t, boot, `-ne '([0-9a-f]{64})'`)
	if got, _ := base64.StdEncoding.DecodeString(file); string(got) != scriptFile {
		t.Errorf("script path %q", got)
	}
	if sum := sha256.Sum256(script); hex.EncodeToString(sum[:]) != hash {
		t.Errorf("the bootstrap's hash is not the install script's")
	}
	if boot != strings.NewReplacer("%FILE%", file, "%HASH%", hash).Replace(psBoot) {
		t.Error("the bootstrap is not the constant one")
	}
	// link 2: the install script is the constant with the plan's path and hash
	pp := regexpFind(t, string(script), `\$planPath = \[System.Text.Encoding\]::UTF8.GetString\(\[System.Convert\]::FromBase64String\('([A-Za-z0-9+/=]+)'\)\)`)
	hh := regexpFind(t, string(script), `\$want = \[System.Text.Encoding\]::UTF8.GetString\(\[System.Convert\]::FromBase64String\('([A-Za-z0-9+/=]+)'\)\)`)
	if got, _ := base64.StdEncoding.DecodeString(pp); string(got) != planFile {
		t.Errorf("plan path %q", got)
	}
	if got, _ := base64.StdEncoding.DecodeString(hh); string(got) != PlanSHA256(p) {
		t.Errorf("plan hash %q, want %q", got, PlanSHA256(p))
	}
	if string(script) != strings.NewReplacer("%PLAN%", pp, "%HASH%", hh).Replace(psInstall) {
		t.Error("the install script is not the constant one")
	}
	// link 3: the plan file is the plan
	if string(plan) != p.FullScript() {
		t.Error("the plan file is not the plan")
	}
	for _, f := range []string{planFile, scriptFile} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s was left behind", f)
		}
	}
}

// The elevated command line (the one ShellExecute gets) must stay short. The bootstrap is what is on it, so the margin does
// not depend on the size of the plugin or of the install script.
func TestUACCommandLinesStayShort(t *testing.T) {
	p := winPlan(t, `C:\Program Files\Rubi Home\rubi-home.exe`)
	inner, err := uacInner(`C:\Users\ann with a long name\AppData\Local\Temp\rubi-home-plan-123456789\plan.json`, PlanSHA256(p))
	if err != nil {
		t.Fatal(err)
	}
	boot, err := uacBoot(`C:\Users\ann with a long name\AppData\Local\Temp\rubi-home-plan-123456789\install.ps1`, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	ps := uacSys + `\WindowsPowerShell\v1.0\powershell.exe`
	elevated := `"` + ps + `" -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand ` + psEncoded(boot)
	launch, _ := uacLaunch(ps, boot)
	relay := `"` + ps + `" -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand ` + psEncoded(launch)
	const shellExecuteSafe, createProcessMax = 2000, 32767
	t.Logf("elevated command line: %d characters (safe limit %d, margin %d); relay: %d (limit %d); install script: %d characters, not on any command line",
		len(elevated), shellExecuteSafe, shellExecuteSafe-len(elevated), len(relay), createProcessMax, len(inner))
	if len(elevated) > shellExecuteSafe {
		t.Errorf("the elevated command line is %d characters, over %d", len(elevated), shellExecuteSafe)
	}
	if len(relay) > createProcessMax/2 {
		t.Errorf("the relay's command line is %d characters", len(relay))
	}
}

// The relay gets a minimal environment: the system's folders and PATH, the user's own folders, nothing else of theirs.
func TestUACEnvironmentIsMinimal(t *testing.T) {
	e := &elevRun{}
	if err := uacRig(e, t.TempDir()).Run(context.Background(), winPlan(t, `C:\Program Files\Rubi Home\rubi-home.exe`)); err != nil {
		t.Fatal(err)
	}
	want := []string{`SystemRoot=C:\Windows`, `windir=C:\Windows`, `PATH=C:\Windows\System32;C:\Windows`, `ComSpec=C:\Windows\System32\cmd.exe`, "PATHEXT=.COM;.EXE;.BAT;.CMD",
		"SystemDrive=C:", `USERPROFILE=C:\Users\ann`, `LOCALAPPDATA=C:\Users\ann\AppData\Local`, `TEMP=C:\Users\ann\AppData\Local\Temp`}
	if !reflect.DeepEqual(e.calls[0].env, want) {
		t.Fatalf("environment %q\nwant        %q", e.calls[0].env, want)
	}
	for _, kv := range e.calls[0].env {
		if strings.HasPrefix(kv, "PSModulePath") || strings.Contains(kv, "evil") {
			t.Errorf("the user's %q reached the relay", kv)
		}
	}
	// a value with a line break is dropped, not passed
	env := minimalEnv(uacSys, envOf("TEMP", "C:\\x\nPATH=C:\\evil", "TMP", `C:\ok`))
	for _, kv := range env {
		if strings.Contains(kv, "evil") {
			t.Errorf("injected: %q", kv)
		}
	}
}

// The order of the elevated script, and the access list it leaves.
func TestUACScriptOrderAndAccessList(t *testing.T) {
	order := []string{"Mutex($false, 'Global\\RubiHomeInstall')", "ReadAllBytes($planPath)", "if ((Get-Sha $raw) -ne $want) { exit 11 }", "ConvertFrom-Json",
		"-ne 'rubi-home.exe') { exit 14 }", "-ne 'claude-plugin') { exit 14 }", "marketplace.json'))) { exit 14 }", "ReadAllBytes($b.src)",
		"if ((Get-Sha $bytes) -ne $b.sha256) { exit 12 }", "WriteAllBytes($tmp, $bytes)", "Lock-Path $tmp $false", "Move-Item -Force -LiteralPath $tmp", "exit 16"}
	last := -1
	for _, want := range order {
		i := strings.Index(psInstall, want)
		if i < 0 || i < last {
			t.Fatalf("%q is missing or out of order in the elevated script", want)
		}
		last = i
	}
	for _, want := range []string{"SetAccessRuleProtection($true, $false)", "'BUILTIN\\Administrators', 'FullControl'", "'NT AUTHORITY\\SYSTEM', 'FullControl'",
		"'BUILTIN\\Users', 'ReadAndExecute'", "SetOwner([System.Security.Principal.NTAccount]'BUILTIN\\Administrators')", "if ((Get-Sha $data) -ne $f.sha256)",
		"$i -lt 5", "Start-Sleep -Seconds 1"} {
		if !strings.Contains(psInstall, want) {
			t.Errorf("the elevated script lacks %q", want)
		}
	}
	for _, bad := range []string{"Everyone", "Authenticated Users", "'Modify'", "'Write'", "Invoke-Expression", "iex ", "Start-Process", "DownloadString", "$args"} {
		if strings.Contains(psInstall, bad) {
			t.Errorf("the elevated script has %q", bad)
		}
	}
	// the bootstrap: the hash is checked before the script runs, and nothing else is in it
	if i, j := strings.Index(psBoot, "if($h -ne '%HASH%'){exit 11}"), strings.Index(psBoot, "Invoke-Expression"); i < 0 || j < 0 || i > j {
		t.Errorf("the bootstrap:\n%s", psBoot)
	}
	if strings.Count(psBoot, "Invoke-Expression") != 1 || strings.Contains(psBoot, "Start-Process") {
		t.Errorf("the bootstrap:\n%s", psBoot)
	}
	if !strings.Contains(psLaunch, "-Verb RunAs") || !strings.Contains(psLaunch, "exit 5") || !strings.Contains(psLaunch, "exit $p.ExitCode") {
		t.Errorf("the launcher:\n%s", psLaunch)
	}
	// the launcher passes the command line of the elevated one as an array: no string is built from parts
	if strings.Contains(psLaunch, "+") || strings.Contains(psLaunch, "-ArgumentList \"") {
		t.Errorf("the launcher composes a command line:\n%s", psLaunch)
	}
}

func TestUACExitStatusesMapToTheClearResults(t *testing.T) {
	for code, check := range map[int]func(error) bool{
		5:  func(err error) bool { return errors.Is(err, ErrAdminCancelled) },
		11: func(err error) bool { return codeOf(err) == CodePlanChanged },
		12: func(err error) bool { return codeOf(err) == CodeVerify },
		13: func(err error) bool { return codeOf(err) == CodeVerify },
		14: func(err error) bool { return codeOf(err) == CodeBadPath },
		15: func(err error) bool { return codeOf(err) == CodeBusy },
		16: func(err error) bool { return codeOf(err) == CodeInUse },
		1:  func(err error) bool { return err != nil && !errors.Is(err, ErrAdminCancelled) && codeOf(err) == "" },
		9:  func(err error) bool { return err != nil && !errors.Is(err, ErrAdminCancelled) },
	} {
		_, ee := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Output()
		if ee == nil {
			t.Skip("no sh")
		}
		e := &elevRun{err: ee}
		err := uacRig(e, t.TempDir()).Run(context.Background(), winPlan(t, `C:\Program Files\Rubi Home\rubi-home.exe`))
		if !check(err) {
			t.Errorf("exit %d: %v", code, err)
		}
	}
	// a timeout of the whole run is the caller's context, not an exit status
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := &elevRun{err: ctx.Err()}
	if err := uacRig(e, t.TempDir()).Run(ctx, winPlan(t, `C:\Program Files\Rubi Home\rubi-home.exe`)); err == nil || errors.Is(err, ErrAdminCancelled) {
		t.Errorf("%v", err)
	}
	// "the hook is running" has its own clear sentence
	if got := hookSentence(CodeInUse); got != "a Claude Code hook is running; close Claude Code and try again" {
		t.Errorf("%q", got)
	}
}

func TestUACRefusesWhatItCannotDo(t *testing.T) {
	e := &elevRun{}
	if err := uacRig(e, t.TempDir()).Run(context.Background(), InstallPlan{Script: "echo hi"}); err == nil || len(e.calls) != 0 {
		t.Errorf("a shell plan: %v %v", err, e.calls)
	}
	noSys := uacElevator(ElevateEnv{Exec: e.exec, LookPath: lookIn()}.withDefaults())
	if err := noSys.Run(context.Background(), winPlan(t, `C:\x\y.exe`)); !errors.Is(err, ErrNoAgent) || len(e.calls) != 0 {
		t.Errorf("no system folder: %v %v", err, e.calls)
	}
	if !errors.Is(noSys.Available(context.Background()), ErrNoAgent) {
		t.Error("available with no system folder")
	}
	noPS := uacElevator(ElevateEnv{Exec: e.exec, SystemDir: uacSys, LookPath: lookIn("powershell.exe")}.withDefaults()) // from PATH: not used
	if err := noPS.Run(context.Background(), winPlan(t, `C:\x\y.exe`)); !errors.Is(err, ErrNoAgent) || len(e.calls) != 0 {
		t.Errorf("PowerShell from PATH: %v %v", err, e.calls)
	}
	if _, err := uacInner(`C:\a\plan.json`, "not a hash"); err == nil {
		t.Error("a non-hash was embedded")
	}
	if _, err := uacBoot(`C:\a\install.ps1`, strings.Repeat("A", 64)); err == nil {
		t.Error("an upper-case hash was embedded in the bootstrap")
	}
	if _, err := uacLaunch(`C:\a'b\powershell.exe`, "x"); err == nil {
		t.Error("a quote in the program path")
	}
}

func TestWindowsPlanDocument(t *testing.T) {
	p := winPlan(t, `C:\Program Files\Rubi Home\rubi-home.exe`)
	full := p.FullScript()
	for _, want := range []string{`{"v":1,"binary":{"src":"C:\\Users\\ann\\AppData\\Local\\Rubi Home\\bin\\rubi-home.exe","dst":"C:\\Program Files\\Rubi Home\\rubi-home.exe","sha256":"`,
		`"plugin":{"dst":"C:\\Program Files\\Rubi Home\\claude-plugin","files":[{"path":"a/b.json","sha256":"aa","data":"e30="}]}}`} {
		if !strings.Contains(full, want) {
			t.Errorf("the plan document lacks %q:\n%s", want, full)
		}
	}
	p.Plugin = nil
	if !strings.HasSuffix(p.FullScript(), `,"plugin":null}`) {
		t.Errorf("without the plugin: %s", p.FullScript())
	}
	for in, bad := range map[string]bool{`C:\Program Files\Rubi Home\x.exe`: false, `C:/x`: true, `relative\x`: true, `C:\a'b`: true, `C:\a"b`: true, `C:\a%PATH%`: true,
		`C:\a$b`: true, `C:\a;b`: true, `C:\a:b`: true, "C:\\a\nb": true, `\\server\share\x`: true, `C:\a` + "`" + `b`: true, `C:\a&b`: true} {
		if got := unsafeWinPath(in) != ""; got != bad {
			t.Errorf("%q: refused=%v", in, got)
		}
	}
}
