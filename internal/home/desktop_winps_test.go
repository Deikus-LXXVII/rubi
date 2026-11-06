package home

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// The programs are those of the System32 folder the system reports, by full path.
const (
	sysDir     = `C:\Windows\System32`
	rundll32   = sysDir + `\rundll32.exe`
	clipExe    = sysDir + `\clip.exe`
	powershell = sysDir + `\WindowsPowerShell\v1.0\powershell.exe`
)

// winRig is the Windows form with a fake runner and every tool present.
func winRig(t *testing.T) (*progRun, Desktop) {
	t.Helper()
	p := &progRun{out: map[string]string{}, err: map[string]error{}}
	return p, winDesktop(DesktopEnv{Run: p.run, LookPath: lookIn(rundll32, clipExe, powershell), SystemDir: sysDir})
}

func TestWindowsOpenArgv(t *testing.T) {
	p, d := winRig(t)
	const link = "https://rubi-panel.com/#home-code=a%2Bb&x=1"
	if err := d.Opener.Open(context.Background(), link); err != nil {
		t.Fatal(err)
	}
	want := []progCall{{"", rundll32, []string{"url.dll,FileProtocolHandler", link}}}
	if !reflect.DeepEqual(p.calls, want) {
		t.Fatalf("%+v", p.calls)
	}
}

func TestWindowsClipboardTakesTheCodeOnStdin(t *testing.T) {
	p, d := winRig(t)
	const code = "rubi-home:SECRET"
	if err := d.Clipboard.Copy(context.Background(), code); err != nil {
		t.Fatal(err)
	}
	if len(p.calls) != 1 || p.calls[0].name != clipExe || p.calls[0].stdin != code || len(p.calls[0].args) != 0 {
		t.Fatalf("%+v", p.calls)
	}
}

// The window's script is a constant; the text, the title and the labels reach it as JSON on standard input, in plain
// ASCII, and appear nowhere in the command line.
func TestWindowsDialogIsArgvOnlyAndTheTextIsOnStdin(t *testing.T) {
	hostile := `x"; Remove-Item -Recurse C:\ ; $(whoami) ` + "`n" + ` 'q' & calc | more > out ` + "\u00e9\u4e2d\U0001F600"
	label := `Open "the" & Rubi $x`
	p, d := winRig(t)
	p.out[powershell] = label + "\r\n"
	got, err := d.Dialogs.Message(context.Background(), hostile, "Close", "Copy", label)
	if err != nil || got != label {
		t.Fatalf("%q %v", got, err)
	}
	c := p.calls[0]
	if c.name != powershell || !reflect.DeepEqual(c.args, psArgs) {
		t.Fatalf("%s %q", c.name, c.args)
	}
	// the argument list is the constant one: no byte of the user's text, no cmd, no /c
	for _, a := range c.args {
		for _, bad := range []string{"Remove-Item", "whoami", "calc", "Open", "Close", "Copy", "Rubi Home", "cmd", "/c"} {
			if a != psDialog && strings.Contains(a, bad) {
				t.Errorf("argument %q holds %q", a, bad)
			}
		}
	}
	if c.args[len(c.args)-1] != psDialog {
		t.Error("the last argument is not the constant script")
	}
	for _, bad := range []string{"Remove-Item", "whoami", "calc", label} {
		if strings.Contains(psDialog, bad) {
			t.Errorf("the script holds %q", bad)
		}
	}
	// stdin: ASCII only, valid JSON, the same values back
	for _, r := range c.stdin {
		if r > 0x7e || (r < 0x20) {
			t.Fatalf("stdin has %U", r)
		}
	}
	var in struct {
		Title   string   `json:"title"`
		Text    string   `json:"text"`
		Buttons []string `json:"buttons"`
	}
	if err := json.Unmarshal([]byte(c.stdin), &in); err != nil {
		t.Fatal(err)
	}
	if in.Text != hostile || in.Title != dialogTitle || !reflect.DeepEqual(in.Buttons, []string{"Close", "Copy", label}) {
		t.Fatalf("%+v", in)
	}
}

func TestWindowsDialogAnswers(t *testing.T) {
	p, d := winRig(t)
	p.out[powershell] = "Copy\r\n"
	if got, err := d.Dialogs.Message(context.Background(), "t", "Close", "Copy", "Open"); err != nil || got != "Copy" {
		t.Fatalf("%q %v", got, err)
	}
	p.out[powershell] = "something else"
	if got, _ := d.Dialogs.Message(context.Background(), "t", "Close", "Copy", "Open"); got != "Close" {
		t.Fatalf("an unknown answer: %q", got)
	}
	p.out[powershell] = ""
	if got, _ := d.Dialogs.Message(context.Background(), "t", "Close", "Copy"); got != "Close" {
		t.Fatalf("no answer: %q", got)
	}
	// the script exits 3 when the session can show no window, and PowerShell may be missing: that is "no desktop"
	p.err[powershell] = exitErr(t, 3)
	if _, err := d.Dialogs.Message(context.Background(), "t", "OK"); !errors.Is(err, ErrNoDesktop) {
		t.Fatalf("exit 3: %v", err)
	}
	p.err[powershell] = exec.ErrNotFound
	if _, err := d.Dialogs.Message(context.Background(), "t", "OK"); !errors.Is(err, ErrNoDesktop) {
		t.Fatalf("not found: %v", err)
	}
	// any other failure is an error, and not a reason to print the pairing code
	for _, e := range []error{exitErr(t, 1), exitErr(t, 2), errors.New("boom")} {
		p.err[powershell] = e
		if _, err := d.Dialogs.Message(context.Background(), "t", "OK"); err == nil || errors.Is(err, ErrNoDesktop) {
			t.Fatalf("%v: %v", e, err)
		}
	}
	if _, err := d.Dialogs.Message(context.Background(), "t"); err == nil {
		t.Error("a dialog with no button")
	}
}

func TestWindowsOffersOnlyWhatIsThere(t *testing.T) {
	d := winDesktop(DesktopEnv{LookPath: lookIn(clipExe), SystemDir: sysDir})
	if d.Opener != nil || d.Dialogs != nil || d.Clipboard == nil {
		t.Fatalf("%+v", d)
	}
}

// What cmd.exe or the quoting of a command line could treat specially never reaches rundll32.
func TestWindowsRefusesALinkWithCharactersThatNeedQuoting(t *testing.T) {
	p, d := winRig(t)
	for _, bad := range []string{"https://x/ y", "https://x/\"y", "https://x/a^b", "https://x/a|b", "https://x/a<b", "https://x/a>b", "https://x/a\\b",
		"https://x/a`b", "https://x/\r\ny", "file:///C:/Windows/System32/calc.exe", "calc.exe", "ms-settings:", "--help", "https://", "ftp://x"} {
		if err := d.Opener.Open(context.Background(), bad); err == nil {
			t.Errorf("%q was opened", bad)
		}
	}
	if len(p.calls) != 0 {
		t.Fatalf("ran for a refused link: %+v", p.calls)
	}
}

func TestAsciiJSON(t *testing.T) {
	got, err := asciiJSON(map[string]any{"a": "x\u00e9\U0001F600\u007f\n\"<>"})
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"a":"x\u00e9\ud83d\ude00\u007f\n\"\u003c\u003e"}` {
		t.Fatal(got)
	}
	var back map[string]string
	if err := json.Unmarshal([]byte(got), &back); err != nil || back["a"] != "x\u00e9\U0001F600\u007f\n\"<>" {
		t.Fatalf("%v %q", err, back)
	}
}

// The script's structure: it reads standard input, never takes text from its arguments, and presses the first button
// for Escape and the close box.
func TestWindowsDialogScriptShape(t *testing.T) {
	for _, want := range []string{"if (-not [Environment]::UserInteractive) { exit 3 }", "$l.UseMnemonic = $false", "$b.UseMnemonic = $false", "[Console]::In.ReadToEnd()", "ConvertFrom-Json", "$f.CancelButton = $b", "$f.AcceptButton = $b", "ShowDialog()", "Write-Output $script:r"} {
		if !strings.Contains(psDialog, want) {
			t.Errorf("the script lacks %q", want)
		}
	}
	for _, bad := range []string{"$args", "Invoke-Expression", "iex ", "Start-Process", "-EncodedCommand", "[Environment]::GetCommandLineArgs"} {
		if strings.Contains(psDialog, bad) {
			t.Errorf("the script uses %q", bad)
		}
	}
}

// Programs come from the System32 folder by full path, never from PATH: with no system folder, nothing is offered;
// a program that is not in it is not offered either, whatever PATH holds.
func TestWindowsProgramsAreTheSystem32Ones(t *testing.T) {
	if d := winDesktop(DesktopEnv{LookPath: lookIn("rundll32.exe", "clip.exe", "powershell.exe")}); d.Opener != nil || d.Clipboard != nil || d.Dialogs != nil {
		t.Fatalf("with no system folder: %+v", d)
	}
	if d := winDesktop(DesktopEnv{LookPath: lookIn("rundll32.exe", "clip.exe", "powershell.exe"), SystemDir: sysDir}); d.Opener != nil || d.Clipboard != nil || d.Dialogs != nil {
		t.Fatalf("programs of PATH were taken: %+v", d)
	}
	p, d := winRig(t)
	_ = d.Opener.Open(context.Background(), "https://rubi-panel.com/")
	_ = d.Clipboard.Copy(context.Background(), "x")
	_, _ = d.Dialogs.Message(context.Background(), "t", "OK")
	for _, c := range p.calls {
		if !strings.HasPrefix(c.name, sysDir+`\`) {
			t.Errorf("ran %q", c.name)
		}
	}
}
