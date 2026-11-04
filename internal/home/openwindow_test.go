package home

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
)

// The pairing window's own logic, on every system, with a fake Desktop. The forms of each system (macOS, Linux,
// Windows) are tested in desktop_*_test.go with fake programs.

type deskCall struct {
	text    string
	buttons []string
}

// fakeDesk is a Desktop whose dialogs are answered from a list (the label of the button "pressed").
type fakeDesk struct {
	answers   []string
	dialogs   []deskCall
	opened    []string
	copied    []string
	openErr   error
	copyErr   error
	dialogErr map[int]error // by dialog number (0 is the first)
	noClip    bool
	noOpen    bool
}

func (f *fakeDesk) desktop() *Desktop {
	d := &Desktop{Dialogs: dialogsFunc(func(_ context.Context, text string, buttons ...string) (string, error) {
		n := len(f.dialogs)
		f.dialogs = append(f.dialogs, deskCall{text, buttons})
		if err := f.dialogErr[n]; err != nil {
			return "", err
		}
		if len(f.answers) == 0 {
			return "", errors.New("unexpected dialog")
		}
		a := f.answers[0]
		f.answers = f.answers[1:]
		return pick(buttons, a), nil // what every form does with a label it does not know
	})}
	if !f.noOpen {
		d.Opener = openerFunc(func(_ context.Context, link string) error {
			f.opened = append(f.opened, link)
			return f.openErr
		})
	}
	if !f.noClip {
		d.Clipboard = clipboardFunc(func(_ context.Context, text string) error {
			f.copied = append(f.copied, text)
			return f.copyErr
		})
	}
	return d
}

func openHelper(t *testing.T) *Helper {
	t.Helper()
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func pending(h *Helper) *Pending { return h.Fresh().Pending }

func setPaired(t *testing.T, h *Helper, label string) {
	t.Helper()
	if err := h.Update(func(c *Config) { c.Paired = []Pairing{{TokenHash: "x", Label: label, At: time.Now()}} }); err != nil {
		t.Fatal(err)
	}
}

func runDesk(t *testing.T, h *Helper, f *fakeDesk) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := RunOpen(context.Background(), h, OpenEnv{Desktop: f.desktop(), Out: &out})
	return out.String(), err
}

func TestOpenLinkCarriesNoSecretAndTheFollowUpShowsThePIN(t *testing.T) {
	h := openHelper(t)
	f := &fakeDesk{answers: []string{btnOpenPanel, btnOK}}
	if _, err := runDesk(t, h, f); err != nil {
		t.Fatal(err)
	}
	if len(f.opened) != 1 {
		t.Fatalf("opened %v", f.opened)
	}
	got := f.opened[0]
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
	follow := f.dialogs[1].text
	for _, want := range []string{"PIN: " + homeproto.FormatPIN(p.Pin), "Pair the waiting Rubi Home", "Fingerprint: " + h.Fingerprint(), "Computer: " + h.Config().Name, "type the PIN", "passkey"} {
		if !strings.Contains(follow, want) {
			t.Fatalf("follow-up %q lacks %q", follow, want)
		}
	}
}

func TestOpenCopyPutsTheFullCodeOnTheClipboardAndInNoDialog(t *testing.T) {
	h := openHelper(t)
	f := &fakeDesk{answers: []string{btnCopy, btnCopyAgain, btnOK}}
	if _, err := runDesk(t, h, f); err != nil {
		t.Fatal(err)
	}
	if len(f.copied) != 2 || f.copied[0] != f.copied[1] {
		t.Fatalf("copied %q", f.copied)
	}
	c, err := homeproto.ParseCode(f.copied[0]) // the full code: it has its own secret
	if err != nil || c.Secret != pending(h).Secret {
		t.Fatalf("the clipboard held %q: %v", f.copied[0], err)
	}
	for _, d := range f.dialogs {
		if strings.Contains(d.text, c.Secret) || strings.Contains(d.text, f.copied[0]) {
			t.Fatalf("the code is in a dialog: %q", d.text)
		}
	}
}

func TestOpenDialogsNameTheComputerAndItsFingerprint(t *testing.T) {
	h := openHelper(t)
	f := &fakeDesk{answers: []string{btnCopy, btnOK}}
	if _, err := runDesk(t, h, f); err != nil {
		t.Fatal(err)
	}
	for i, c := range f.dialogs {
		for _, want := range []string{"Computer: " + h.Config().Name, "Fingerprint: " + h.Fingerprint()} {
			if !strings.Contains(c.text, want) {
				t.Fatalf("dialog %d %q lacks %q", i, c.text, want)
			}
		}
	}
	if !strings.Contains(f.dialogs[0].text, "The code works once, for 15 minutes.") {
		t.Fatal(f.dialogs[0].text)
	}
	// the first dialog: Close is first (it is what Escape does), the last is the default
	if got := f.dialogs[0].buttons; len(got) != 3 || got[0] != btnClose || got[1] != btnCopy || got[2] != btnOpenPanel {
		t.Fatalf("buttons %v", got)
	}
}

func TestOpenCloseLeavesNoLiveSecret(t *testing.T) {
	h := openHelper(t)
	f := &fakeDesk{answers: []string{btnClose}}
	if _, err := runDesk(t, h, f); err != nil {
		t.Fatal(err)
	}
	if len(f.dialogs) != 1 || len(f.opened)+len(f.copied) != 0 {
		t.Fatalf("after Close: %d dialogs, %v %v", len(f.dialogs), f.opened, f.copied)
	}
	if pending(h) != nil {
		t.Fatal("Close left a pending code")
	}
}

func TestOpenReusesAStillGoodPendingCode(t *testing.T) {
	h := openHelper(t)
	if _, err := runDesk(t, h, &fakeDesk{answers: []string{btnCopy, btnOK}}); err != nil {
		t.Fatal(err)
	}
	first := *pending(h)
	if _, err := runDesk(t, h, &fakeDesk{answers: []string{btnCopy, btnOK}}); err != nil {
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
	if _, err := runDesk(t, h, &fakeDesk{answers: []string{btnCopy, btnOK}}); err != nil {
		t.Fatal(err)
	}
	if got := pending(h); got.Secret == first.Secret || time.Until(got.Expires) < 14*time.Minute {
		t.Fatalf("a nearly expired code was reused: %+v", got)
	}
}

func TestOpenMakesAOneTimeCodeLikePairDoes(t *testing.T) {
	h := openHelper(t)
	if _, err := runDesk(t, h, &fakeDesk{answers: []string{btnCopy, btnOK}}); err != nil {
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
	f := &fakeDesk{answers: []string{btnClose}}
	if _, err := runDesk(t, h, f); err != nil {
		t.Fatal(err)
	}
	msg := f.dialogs[0].text
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
	f := &fakeDesk{answers: []string{btnClose}}
	if _, err := runDesk(t, h, f); err != nil {
		t.Fatal(err)
	}
	if msg := f.dialogs[0].text; !strings.HasPrefix(msg, "Paired with Rubi since ") {
		t.Fatal(msg)
	}
}

func TestOpenPairAgainMakesACodeOnlyWhenAsked(t *testing.T) {
	h := openHelper(t)
	setPaired(t, h, "Rubi")
	if _, err := runDesk(t, h, &fakeDesk{answers: []string{btnPairAgain, btnClose}}); err != nil {
		t.Fatal(err)
	}
	if pending(h) != nil {
		t.Fatal("Pair again, then Close, left a code")
	}
	f := &fakeDesk{answers: []string{btnPairAgain, btnCopy, btnOK}}
	if _, err := runDesk(t, h, f); err != nil {
		t.Fatal(err)
	}
	if pending(h) == nil {
		t.Fatal("Pair again, then Copy made no pending code")
	}
	if len(f.copied) != 1 || !strings.HasPrefix(f.copied[0], "rubi-home:") {
		t.Fatalf("copied %q", f.copied)
	}
}

func TestOpenAnUnknownButtonIsTreatedAsClose(t *testing.T) {
	h := openHelper(t)
	f := &fakeDesk{answers: []string{"Something else"}}
	if _, err := runDesk(t, h, f); err != nil {
		t.Fatal(err)
	}
	if len(f.dialogs) != 1 || pending(h) != nil {
		t.Fatalf("dialogs %d", len(f.dialogs))
	}
}

func TestOpenReportsAProgramThatFails(t *testing.T) {
	for _, what := range []string{"open", "copy"} {
		h := openHelper(t)
		f := &fakeDesk{answers: []string{btnOpenPanel, btnOK}}
		if what == "copy" {
			f = &fakeDesk{answers: []string{btnCopy, btnOK}, copyErr: errors.New("no")}
		} else {
			f.openErr = errors.New("no")
		}
		if _, err := runDesk(t, h, f); err == nil {
			t.Fatalf("%s failed silently", what)
		}
		if len(f.dialogs) != 1 {
			t.Fatalf("%s: a follow-up said it worked", what)
		}
	}
}

// A computer with no clipboard tool offers Open only, and one with no browser opener offers Copy only; with neither,
// a window could do nothing, so the text is printed.
func TestOpenOffersOnlyWhatTheComputerCanDo(t *testing.T) {
	h := openHelper(t)
	f := &fakeDesk{answers: []string{btnClose}, noClip: true}
	if _, err := runDesk(t, h, f); err != nil {
		t.Fatal(err)
	}
	if got := f.dialogs[0].buttons; strings.Join(got, "|") != btnClose+"|"+btnOpenPanel {
		t.Fatalf("no clipboard: buttons %v", got)
	}
	f = &fakeDesk{answers: []string{btnClose}, noOpen: true}
	if _, err := runDesk(t, h, f); err != nil {
		t.Fatal(err)
	}
	if got := f.dialogs[0].buttons; strings.Join(got, "|") != btnClose+"|"+btnCopy {
		t.Fatalf("no opener: buttons %v", got)
	}
	f = &fakeDesk{answers: []string{btnOpenPanel, btnOK}, noClip: true}
	if _, err := runDesk(t, h, f); err != nil {
		t.Fatal(err)
	}
	if got := f.dialogs[1].buttons; len(got) != 1 || got[0] != btnOK {
		t.Fatalf("no clipboard: the follow-up offers %v", got)
	}
	f = &fakeDesk{noClip: true, noOpen: true}
	out, err := runDesk(t, h, f)
	if err != nil || len(f.dialogs) != 0 || !strings.Contains(out, "https://rubi-panel.com/#home-code=") {
		t.Fatalf("neither: %d dialogs, output %q, %v", len(f.dialogs), out, err)
	}
}

func checkTextMode(t *testing.T, h *Helper, s string) {
	t.Helper()
	p := pending(h)
	if p == nil {
		t.Fatal("no pending code")
	}
	if !strings.Contains(s, "https://rubi-panel.com/#home-code=") || !strings.Contains(s, homeproto.FormatPIN(p.Pin)) ||
		!strings.Contains(s, "rubi-home:") || !strings.Contains(s, h.Fingerprint()) || !strings.Contains(s, h.Config().Name) {
		t.Fatalf("output %q", s)
	}
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "https://") && (strings.Contains(l, p.Secret) || strings.Contains(l, p.Pin)) {
			t.Fatalf("a secret in the link: %q", l)
		}
	}
}

// With no desktop at all (a box with no display, a session that can show nothing) the link, the PIN and the code are
// printed, for whoever runs Rubi Home to pass on.
func TestOpenWithoutADesktopPrintsTheLinkThePINAndTheCode(t *testing.T) {
	h := openHelper(t)
	var out bytes.Buffer
	if err := RunOpen(context.Background(), h, OpenEnv{Desktop: &Desktop{}, Out: &out}); err != nil {
		t.Fatal(err)
	}
	checkTextMode(t, h, out.String())
}

// A dialog that can not be shown (the form reports ErrNoDesktop) ends in the same text, at either dialog.
func TestOpenFallsBackToTextWhenTheWindowCannotBeShown(t *testing.T) {
	h := openHelper(t)
	f := &fakeDesk{dialogErr: map[int]error{0: ErrNoDesktop}}
	out, err := runDesk(t, h, f)
	if err != nil {
		t.Fatal(err)
	}
	checkTextMode(t, h, out)
	h2 := openHelper(t)
	setPaired(t, h2, "Rubi")
	f = &fakeDesk{dialogErr: map[int]error{0: errors.Join(ErrNoDesktop, errors.New("powershell"))}}
	out, err = runDesk(t, h2, f)
	if err != nil {
		t.Fatal(err)
	}
	checkTextMode(t, h2, out)
	// another failure is an error, not a reason to print a secret
	h3 := openHelper(t)
	out3, err := runDesk(t, h3, &fakeDesk{dialogErr: map[int]error{0: errors.New("boom")}})
	if err == nil || pending(h3) != nil {
		t.Fatalf("a real failure: %v, pending %v", err, pending(h3))
	}
	if strings.Contains(out3, "rubi-home:") || strings.Contains(out3, "home-code") || !strings.Contains(out3, PanelURL) {
		t.Fatalf("a failed window printed %q: no code, but the panel's address", out3)
	}
	// ... also when the pairing dialog is the second one (an already paired computer)
	h4 := openHelper(t)
	setPaired(t, h4, "Rubi")
	out4, err := runDesk(t, h4, &fakeDesk{dialogErr: map[int]error{0: errors.New("boom")}})
	if err == nil || pending(h4) != nil || strings.Contains(out4, "rubi-home:") {
		t.Fatalf("paired: %v %q", err, out4)
	}
}

// The system's own Desktop is what a plain OpenEnv uses (no fake): on this computer it can be anything, but it can
// not panic and it must answer for each part.
func TestOpenEnvBuildsTheSystemsDesktop(t *testing.T) {
	d := OpenEnv{Getenv: func(string) string { return "" }, LookPath: func(string) (string, error) { return "", errors.New("none") },
		Run: func(context.Context, string, string, ...string) (string, error) {
			return "", errors.New("must not run")
		}}.desktop()
	_ = d // every part may be nil here; building it ran nothing
}

func TestPairingLinkEscapes(t *testing.T) {
	if got := PairingLink("a b+c/d:e"); got != PanelURL+"#home-code=a+b%2Bc%2Fd%3Ae" {
		t.Fatal(got)
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
