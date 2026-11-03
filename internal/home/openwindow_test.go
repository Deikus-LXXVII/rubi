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
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
)

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
		if c.args[i] != "-e" {
			return c.args[i]
		}
		i++
	}
	return ""
}

func openHelper(t *testing.T) *Helper {
	t.Helper()
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func runOpen(t *testing.T, h *Helper, f *fakeRun) error {
	t.Helper()
	return RunOpen(context.Background(), h, OpenEnv{Run: f.run, Out: &bytes.Buffer{}, GOOS: "darwin"})
}

func pending(h *Helper) *Pending { return h.Fresh().Pending }

func setPaired(t *testing.T, h *Helper, label string) {
	t.Helper()
	if err := h.Update(func(c *Config) { c.Paired = []Pairing{{TokenHash: "x", Label: label, At: time.Now()}} }); err != nil {
		t.Fatal(err)
	}
}

func TestOpenLinkCarriesNoSecretAndTheFollowUpShowsThePIN(t *testing.T) {
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

func TestOpenCopyPutsTheFullCodeOnStdinNotArgv(t *testing.T) {
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

func TestOpenDialogsNameTheComputerAndItsFingerprint(t *testing.T) {
	h := openHelper(t)
	f := &fakeRun{answers: []string{btnCopy, btnOK}}
	if err := runOpen(t, h, f); err != nil {
		t.Fatal(err)
	}
	for i, c := range f.named("/usr/bin/osascript") {
		text := dialogText(c)
		for _, want := range []string{"Computer: " + h.Config().Name, "Fingerprint: " + h.Fingerprint()} {
			if !strings.Contains(text, want) {
				t.Fatalf("dialog %d %q lacks %q", i, text, want)
			}
		}
	}
	if first := dialogText(f.calls[0]); !strings.Contains(first, "The code works once, for 15 minutes.") {
		t.Fatal(first)
	}
}

func TestOpenCloseLeavesNoLiveSecret(t *testing.T) {
	h := openHelper(t)
	f := &fakeRun{answers: []string{btnClose}}
	if err := runOpen(t, h, f); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls after Close: %d", len(f.calls))
	}
	if pending(h) != nil {
		t.Fatal("Close left a pending code")
	}
}

func TestOpenReusesAStillGoodPendingCode(t *testing.T) {
	h := openHelper(t)
	if err := runOpen(t, h, &fakeRun{answers: []string{btnCopy, btnOK}}); err != nil {
		t.Fatal(err)
	}
	first := *pending(h)
	if err := runOpen(t, h, &fakeRun{answers: []string{btnCopy, btnOK}}); err != nil {
		t.Fatal(err)
	}
	if got := *pending(h); got != first {
		t.Fatalf("a second open replaced the code: %+v then %+v", first, got)
	}
	// a worn-out one is replaced
	if err := h.Update(func(c *Config) { c.Pending.Attempts = maxPairAttempts - 1 }); err != nil {
		t.Fatal(err)
	}
	if err := h.Update(func(c *Config) { c.Pending.Expires = time.Now().Add(10 * time.Second) }); err != nil {
		t.Fatal(err)
	}
	if err := runOpen(t, h, &fakeRun{answers: []string{btnCopy, btnOK}}); err != nil {
		t.Fatal(err)
	}
	if got := pending(h); got.Secret == first.Secret || time.Until(got.Expires) < 14*time.Minute {
		t.Fatalf("a nearly expired code was reused: %+v", got)
	}
}

func TestOpenMakesAOneTimeCodeLikePairDoes(t *testing.T) {
	h := openHelper(t)
	if err := runOpen(t, h, &fakeRun{answers: []string{btnCopy, btnOK}}); err != nil {
		t.Fatal(err)
	}
	p := pending(h)
	if p == nil || p.Secret == "" {
		t.Fatal("no pending secret: a running helper would refuse the code")
	}
	if d := time.Until(p.Expires); d < 14*time.Minute || d > 15*time.Minute+time.Second {
		t.Fatalf("expires in %v", d)
	}
}

func TestOpenPairedShowsLabelDateAndFingerprint(t *testing.T) {
	h := openHelper(t)
	at := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	if err := h.Update(func(c *Config) { c.Paired = []Pairing{{TokenHash: "x", Label: "Rubi abc", At: at}} }); err != nil {
		t.Fatal(err)
	}
	f := &fakeRun{answers: []string{btnClose}}
	if err := runOpen(t, h, f); err != nil {
		t.Fatal(err)
	}
	msg := dialogText(f.calls[0])
	for _, want := range []string{"Paired with Rubi abc since ", at.Local().Format("2 January 2006"), "Fingerprint " + h.Fingerprint(), "shows the same when you pair"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("%q lacks %q", msg, want)
		}
	}
	if pending(h) != nil {
		t.Fatal("a paired Close made a code")
	}
}

func TestOpenPairedWithoutALabelSaysRubi(t *testing.T) {
	h := openHelper(t)
	setPaired(t, h, "  ")
	f := &fakeRun{answers: []string{btnClose}}
	if err := runOpen(t, h, f); err != nil {
		t.Fatal(err)
	}
	if msg := dialogText(f.calls[0]); !strings.HasPrefix(msg, "Paired with Rubi since ") {
		t.Fatal(msg)
	}
}

func TestOpenPairAgainMakesACodeOnlyWhenAsked(t *testing.T) {
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

func TestOpenNeverSplicesTextIntoAppleScript(t *testing.T) {
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

func TestOpenEscapeIsCloseAlsoForARealExitError(t *testing.T) {
	// the real exec runner, with a program that fails the way osascript does when the user presses Escape
	run := func(ctx context.Context, stdin, name string, args ...string) (string, error) {
		if name != "/usr/bin/osascript" {
			t.Fatalf("ran %s", name)
		}
		return execRun(ctx, stdin, "/bin/sh", "-c", `echo "execution error: User canceled. (-128)" >&2; exit 1`)
	}
	h := openHelper(t)
	if err := RunOpen(context.Background(), h, OpenEnv{Run: run, Out: &bytes.Buffer{}, GOOS: "darwin"}); err != nil {
		t.Fatalf("Escape is an error: %v", err)
	}
	if pending(h) != nil {
		t.Fatal("Escape left a code")
	}
	// another failure of the same program is an error
	bad := func(ctx context.Context, stdin, name string, args ...string) (string, error) {
		return execRun(ctx, stdin, "/bin/sh", "-c", `echo "boom" >&2; exit 1`)
	}
	if err := RunOpen(context.Background(), h, OpenEnv{Run: bad, Out: &bytes.Buffer{}, GOOS: "darwin"}); err == nil {
		t.Fatal("a real failure was taken for Escape")
	}
}

func TestOpenCancelInTheFollowUpEndsQuietly(t *testing.T) {
	h := openHelper(t)
	f := &fakeRun{answers: []string{btnOpenPanel}, dialogErr: map[int]error{1: errors.New("User canceled. (-128)")}}
	if err := runOpen(t, h, f); err != nil {
		t.Fatal(err)
	}
	if len(f.named("/usr/bin/open")) != 1 || pending(h) == nil {
		t.Fatal("the code made before the follow-up should stay")
	}
}

func TestOpenAnUnknownButtonIsTreatedAsClose(t *testing.T) {
	h := openHelper(t)
	f := &fakeRun{answers: []string{"Something else"}}
	if err := runOpen(t, h, f); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || pending(h) != nil {
		t.Fatalf("calls %d", len(f.calls))
	}
}

func TestOpenReportsAProgramThatFails(t *testing.T) {
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

func TestOpenNotDarwinPrintsTheLinkThePINAndTheCode(t *testing.T) {
	h := openHelper(t)
	var out bytes.Buffer
	f := &fakeRun{}
	if err := RunOpen(context.Background(), h, OpenEnv{Run: f.run, Out: &out, GOOS: "linux"}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatal("ran a program off macOS")
	}
	p := pending(h)
	s := out.String()
	if !strings.Contains(s, "https://rubi-panel.com/#home-code=") || !strings.Contains(s, homeproto.FormatPIN(p.Pin)) ||
		!strings.Contains(s, "rubi-home:") || !strings.Contains(s, h.Fingerprint()) {
		t.Fatalf("output %q", s)
	}
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "https://") && (strings.Contains(l, p.Secret) || strings.Contains(l, p.Pin)) {
			t.Fatalf("a secret in the link: %q", l)
		}
	}
}

func TestPairingLinkEscapes(t *testing.T) {
	if got := PairingLink("a b+c/d:e"); got != PanelURL+"#home-code=a+b%2Bc%2Fd%3Ae" {
		t.Fatal(got)
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
		"dialog": dialogArgs("text", "Close", "Copy", "Open"),
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

// Two Helpers on one folder (the CLI and the running helper are two processes; flock orders both the same way)
// don't lose each other's changes.
func TestUpdateKeepsBothWritersChanges(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const n = 40
	var wg sync.WaitGroup
	for _, w := range []struct {
		h   *Helper
		pre string
	}{{a, "a"}, {b, "b"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < n; i++ {
				key := w.pre + string(rune('A'+i%26)) + string(rune('a'+i/26))
				if err := w.h.Update(func(c *Config) {
					if c.HuePins == nil {
						c.HuePins = map[string]string{}
					}
					c.HuePins[key] = "x"
				}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if got := len(a.Fresh().HuePins); got != 2*n {
		t.Fatalf("%d of %d changes survived", got, 2*n)
	}
}
