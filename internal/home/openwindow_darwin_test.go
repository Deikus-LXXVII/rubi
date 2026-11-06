//go:build darwin

package home

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
)

// The macOS form of the pairing window, with a fake osascript, open and pbcopy: the argv of each, what is never
// spliced into a script, and how Escape (error -128) is told from a real failure. The window's own logic is tested
// on every system in openwindow_test.go with a fake Desktop.

type runCall struct {
	stdin string
	name  string
	args  []string
}

// fakeRun plays osascript: it answers each dialog with the next of answers (the button's label), and records
// every call.
type fakeRun struct {
	answers   []string
	calls     []runCall
	fail      map[string]error // by program
	dialogErr map[int]error    // by dialog number (0 is the first)
	dialogs   int
}

func (f *fakeRun) run(_ context.Context, stdin, name string, args ...string) (string, error) {
	f.calls = append(f.calls, runCall{stdin, name, args})
	if err := f.fail[name]; err != nil {
		return "", err
	}
	if name == "/usr/bin/osascript" {
		n := f.dialogs
		f.dialogs++
		if err := f.dialogErr[n]; err != nil {
			return "", err
		}
		if len(f.answers) == 0 {
			return "", errors.New("unexpected dialog")
		}
		a := f.answers[0]
		f.answers = f.answers[1:]
		return a + "\n", nil
	}
	return "", nil
}

func (f *fakeRun) named(name string) []runCall {
	var out []runCall
	for _, c := range f.calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

// text is a dialog's message: the first argument after the script lines.
func dialogText(c runCall) string {
	for i := 0; i+1 < len(c.args); i++ {
		if c.args[i] == "--" {
			return c.args[i+1]
		}
		if c.args[i] != "-e" {
			return c.args[i]
		}
		i++
	}
	return ""
}

func runOpen(t *testing.T, h *Helper, f *fakeRun) error {
	t.Helper()
	return RunOpen(context.Background(), h, OpenEnv{Run: f.run, Out: &bytes.Buffer{}})
}

func TestMacOpenLinkCarriesNoSecretAndTheFollowUpShowsThePIN(t *testing.T) {
	h := openHelper(t)
	f := &fakeRun{answers: []string{btnOpenPanel, btnOK}}
	if err := runOpen(t, h, f); err != nil {
		t.Fatal(err)
	}
	opens := f.named("/usr/bin/open")
	if len(opens) != 1 || len(opens[0].args) != 1 {
		t.Fatalf("open calls: %+v", opens)
	}
	got := opens[0].args[0]
	if !strings.HasPrefix(got, "https://rubi-panel.com/#home-code=") {
		t.Fatalf("link %q", got)
	}
	raw := strings.TrimPrefix(got, "https://rubi-panel.com/#home-code=")
	if strings.ContainsAny(raw, ":+ ") {
		t.Fatalf("code not escaped: %q", raw)
	}
	dec, err := url.QueryUnescape(raw)
	if err != nil || !strings.HasPrefix(dec, "rubi-home:") {
		t.Fatalf("the fragment must hold the escaped code: %q (%v)", raw, err)
	}
	p := pending(h)
	if p == nil || p.Pin == "" {
		t.Fatal("no pending code")
	}
	if strings.Contains(got, p.Secret) || strings.Contains(got, p.Pin) {
		t.Fatal("a secret is in the URL")
	}
	c, err := homeproto.ParseCodeWithPin(dec, homeproto.FormatPIN(p.Pin))
	if err != nil || c.Secret != p.Pin || c.Name != h.Config().Name {
		t.Fatalf("the link's code with the PIN: %+v %v", c, err)
	}
	if _, err := homeproto.ParseCode(dec); err == nil {
		t.Fatal("the link's code works alone")
	}
	follow := dialogText(f.named("/usr/bin/osascript")[1])
	for _, want := range []string{"PIN: " + homeproto.FormatPIN(p.Pin), "Pair the waiting Rubi Home", "Fingerprint: " + h.Fingerprint(), "Computer: " + h.Config().Name, "type the PIN", "passkey"} {
		if !strings.Contains(follow, want) {
			t.Fatalf("follow-up %q lacks %q", follow, want)
		}
	}
}

func TestMacOpenCopyPutsTheFullCodeOnStdinNotArgv(t *testing.T) {
	h := openHelper(t)
	f := &fakeRun{answers: []string{btnCopy, btnCopyAgain, btnOK}}
	if err := runOpen(t, h, f); err != nil {
		t.Fatal(err)
	}
	cp := f.named("/usr/bin/pbcopy")
	if len(cp) != 2 {
		t.Fatalf("pbcopy calls: %d", len(cp))
	}
	c, err := homeproto.ParseCode(cp[0].stdin) // the full code: it has its own secret
	if err != nil || c.Secret != pending(h).Secret || cp[0].stdin != cp[1].stdin {
		t.Fatalf("stdin %q: %v", cp[0].stdin, err)
	}
	for _, call := range f.calls {
		for _, a := range call.args {
			if strings.Contains(a, cp[0].stdin) || strings.Contains(a, c.Secret) {
				t.Fatalf("the code is in argv of %s", call.name)
			}
		}
	}
	if len(cp[0].args) != 0 {
		t.Fatalf("pbcopy argv %v", cp[0].args)
	}
}

func TestMacOpenPairAgainMakesACodeOnlyWhenAsked(t *testing.T) {
	h := openHelper(t)
	setPaired(t, h, "Rubi")
	f := &fakeRun{answers: []string{btnPairAgain, btnClose}}
	if err := runOpen(t, h, f); err != nil {
		t.Fatal(err)
	}
	if pending(h) != nil {
		t.Fatal("Pair again, then Close, left a code")
	}
	f = &fakeRun{answers: []string{btnPairAgain, btnCopy, btnOK}}
	if err := runOpen(t, h, f); err != nil {
		t.Fatal(err)
	}
	if pending(h) == nil {
		t.Fatal("Pair again, then Copy made no pending code")
	}
	if cp := f.named("/usr/bin/pbcopy"); len(cp) != 1 || !strings.HasPrefix(cp[0].stdin, "rubi-home:") {
		t.Fatalf("pbcopy %+v", cp)
	}
}

func TestMacOpenNeverSplicesTextIntoAppleScript(t *testing.T) {
	h := openHelper(t)
	setPaired(t, h, `x" & (do shell script "id") & "`)
	f := &fakeRun{answers: []string{btnPairAgain, btnOpenPanel, btnOK}}
	if err := runOpen(t, h, f); err != nil {
		t.Fatal(err)
	}
	scripts := f.named("/usr/bin/osascript")
	if len(scripts) != 3 {
		t.Fatalf("dialogs: %d", len(scripts))
	}
	for _, c := range scripts {
		for i := 0; i+1 < len(c.args) && c.args[i] == "-e"; i += 2 {
			line := c.args[i+1]
			for _, bad := range []string{"do shell script", "Paired", "rubi-home:", "Pair this", "PIN:"} {
				if strings.Contains(line, bad) {
					t.Fatalf("text spliced into the script: %q", line)
				}
			}
		}
	}
	if !strings.Contains(strings.Join(scripts[0].args, "\n"), `do shell script "id"`) {
		t.Fatal("label not passed as an argument")
	}
}

func TestMacOpenEscapeIsCloseAlsoForARealExitError(t *testing.T) {
	// the real exec runner, with a program that fails the way osascript does when the user presses Escape
	run := func(ctx context.Context, stdin, name string, args ...string) (string, error) {
		if name != "/usr/bin/osascript" {
			t.Fatalf("ran %s", name)
		}
		return execRun(ctx, stdin, "/bin/sh", "-c", `echo "execution error: User canceled. (-128)" >&2; exit 1`)
	}
	h := openHelper(t)
	if err := RunOpen(context.Background(), h, OpenEnv{Run: run, Out: &bytes.Buffer{}}); err != nil {
		t.Fatalf("Escape is an error: %v", err)
	}
	if pending(h) != nil {
		t.Fatal("Escape left a code")
	}
	// another failure of the same program is an error
	bad := func(ctx context.Context, stdin, name string, args ...string) (string, error) {
		return execRun(ctx, stdin, "/bin/sh", "-c", `echo "boom" >&2; exit 1`)
	}
	if err := RunOpen(context.Background(), h, OpenEnv{Run: bad, Out: &bytes.Buffer{}}); err == nil {
		t.Fatal("a real failure was taken for Escape")
	}
}

func TestMacOpenCancelInTheFollowUpEndsQuietly(t *testing.T) {
	h := openHelper(t)
	f := &fakeRun{answers: []string{btnOpenPanel}, dialogErr: map[int]error{1: errors.New("User canceled. (-128)")}}
	if err := runOpen(t, h, f); err != nil {
		t.Fatal(err)
	}
	if len(f.named("/usr/bin/open")) != 1 || pending(h) == nil {
		t.Fatal("the code made before the follow-up should stay")
	}
}

func TestMacOpenAnUnknownButtonIsTreatedAsClose(t *testing.T) {
	h := openHelper(t)
	f := &fakeRun{answers: []string{"Something else"}}
	if err := runOpen(t, h, f); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || pending(h) != nil {
		t.Fatalf("calls %d", len(f.calls))
	}
}

func TestMacOpenReportsAProgramThatFails(t *testing.T) {
	for _, prog := range []string{"/usr/bin/open", "/usr/bin/pbcopy"} {
		h := openHelper(t)
		answer := btnOpenPanel
		if prog == "/usr/bin/pbcopy" {
			answer = btnCopy
		}
		f := &fakeRun{answers: []string{answer, btnOK}, fail: map[string]error{prog: errors.New("no")}}
		if err := runOpen(t, h, f); err == nil {
			t.Fatalf("%s failed silently", prog)
		}
		if len(f.named("/usr/bin/osascript")) != 1 {
			t.Fatalf("%s: a follow-up said it worked", prog)
		}
	}
}

// The scripts are compiled, never run: a syntax error in them would only show on a Mac otherwise.
func TestOsascriptScriptsCompile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("osacompile is macOS only")
	}
	if _, err := exec.LookPath("/usr/bin/osacompile"); err != nil {
		t.Skip("no osacompile")
	}
	lines := func(args []string) []string {
		var out []string
		for i := 0; i+1 < len(args) && args[i] == "-e"; i += 2 {
			out = append(out, "-e", args[i+1])
		}
		return out
	}
	for name, args := range map[string][]string{
		"dialog": dialogScriptArgs(t),
		"admin":  osascriptArgs(InstallPlan{}),
	} {
		dir := t.TempDir()
		cmd := exec.Command("/usr/bin/osacompile", append([]string{"-o", filepath.Join(dir, name+".scpt")}, lines(args)...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s script does not compile: %v: %s", name, err, out)
		} else if _, err := os.Stat(filepath.Join(dir, name+".scpt")); err != nil {
			t.Errorf("%s: no output", name)
		}
	}
}

// dialogScriptArgs is the argv the macOS dialog hands to osascript, caught with a fake runner.
func dialogScriptArgs(t *testing.T) []string {
	t.Helper()
	var got []string
	d := platformDesktop(DesktopEnv{Run: func(_ context.Context, _ string, name string, args ...string) (string, error) {
		got = args
		return "Close", nil
	}})
	if _, err := d.Dialogs.Message(context.Background(), "text", "Close", "Copy", "Open"); err != nil {
		t.Fatal(err)
	}
	return got
}

// The text and the buttons come after "--", so that a text that starts with a dash is never an option of osascript.
func TestMacDialogPutsDashDashBeforeTheText(t *testing.T) {
	for _, text := range []string{"-e display dialog 1", "--help", "-l JavaScript"} {
		args := dialogScriptArgsFor(t, text)
		at := -1
		for i, a := range args {
			if a == "--" {
				at = i
			}
		}
		if at < 0 || args[at+1] != text || args[at+2] != dialogTitle || at+3+3 != len(args) {
			t.Errorf("%q: %q", text, args)
		}
		for i := 0; i < at; i++ {
			if args[i] == text {
				t.Errorf("the text is before --: %q", args)
			}
		}
	}
}

func dialogScriptArgsFor(t *testing.T, text string) []string {
	t.Helper()
	var got []string
	d := platformDesktop(DesktopEnv{Run: func(_ context.Context, _ string, _ string, args ...string) (string, error) {
		got = args
		return "Close", nil
	}})
	if _, err := d.Dialogs.Message(context.Background(), text, "Close", "Copy", "Open"); err != nil {
		t.Fatal(err)
	}
	return got
}
