package home

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// progRun is a fake runner for the forms of a system: it records each call and answers from a table by program.
type progCall struct {
	stdin string
	name  string
	args  []string
}

type progRun struct {
	calls []progCall
	out   map[string]string
	err   map[string]error
}

func (p *progRun) run(_ context.Context, stdin, name string, args ...string) (string, error) {
	p.calls = append(p.calls, progCall{stdin, name, args})
	return p.out[name], p.err[name]
}

func lookIn(have ...string) func(string) (string, error) {
	return func(n string) (string, error) {
		for _, h := range have {
			if h == n {
				return "/bin/" + n, nil
			}
		}
		return "", exec.ErrNotFound
	}
}

func envOf(kv ...string) func(string) string {
	return func(k string) string {
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i] == k {
				return kv[i+1]
			}
		}
		return ""
	}
}

// exitErr is the error of a program that ended with a status, as exec reports it.
func exitErr(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+string(rune('0'+code))).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Skip("no sh to make an exit status")
	}
	return err
}

func TestXDGNeedsADisplay(t *testing.T) {
	d := xdgDesktop(DesktopEnv{LookPath: lookIn("xdg-open", "zenity", "wl-copy"), Getenv: envOf()})
	if d.Opener != nil || d.Clipboard != nil || d.Dialogs != nil {
		t.Fatalf("a box with no display got %+v", d)
	}
}

func TestXDGOpensWithXdgOpenAndOneArgument(t *testing.T) {
	p := &progRun{}
	d := xdgDesktop(DesktopEnv{Run: p.run, LookPath: lookIn("xdg-open"), Getenv: envOf("DISPLAY", ":0")})
	if err := d.Opener.Open(context.Background(), "https://rubi-panel.com/#home-code=a%2Bb"); err != nil {
		t.Fatal(err)
	}
	want := []progCall{{"", "xdg-open", []string{"https://rubi-panel.com/#home-code=a%2Bb"}}}
	if !reflect.DeepEqual(p.calls, want) {
		t.Fatalf("%+v", p.calls)
	}
	if d.Clipboard != nil || d.Dialogs != nil {
		t.Fatal("tools that are not there were offered")
	}
}

func TestXDGClipboardPicksTheToolOfTheSession(t *testing.T) {
	for _, c := range []struct {
		name string
		have []string
		env  []string
		want []string // program and argv
	}{
		{"wayland", []string{"wl-copy", "xclip"}, []string{"WAYLAND_DISPLAY", "wayland-0", "DISPLAY", ":0"}, []string{"wl-copy"}},
		{"x11 with xclip", []string{"xclip", "xsel"}, []string{"DISPLAY", ":0"}, []string{"xclip", "-selection", "clipboard", "-in"}},
		{"x11 with xsel", []string{"xsel"}, []string{"DISPLAY", ":0"}, []string{"xsel", "--clipboard", "--input"}},
		{"wayland without wl-copy, with xclip (XWayland)", []string{"xclip"}, []string{"WAYLAND_DISPLAY", "w", "DISPLAY", ":1"}, []string{"xclip", "-selection", "clipboard", "-in"}},
	} {
		p := &progRun{}
		d := xdgDesktop(DesktopEnv{Run: p.run, LookPath: lookIn(c.have...), Getenv: envOf(c.env...)})
		if d.Clipboard == nil {
			t.Errorf("%s: no clipboard", c.name)
			continue
		}
		const code = "rubi-home:SECRET==/+"
		if err := d.Clipboard.Copy(context.Background(), code); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		want := []progCall{{code, c.want[0], c.want[1:]}}
		if len(c.want) == 1 {
			want[0].args = nil
		}
		if len(p.calls) != 1 || p.calls[0].stdin != code || p.calls[0].name != c.want[0] || strings.Join(p.calls[0].args, " ") != strings.Join(c.want[1:], " ") {
			t.Errorf("%s: %+v, want %+v", c.name, p.calls, want)
		}
		for _, a := range p.calls[0].args {
			if strings.Contains(a, "SECRET") {
				t.Errorf("%s: the code is in argv: %v", c.name, p.calls[0].args)
			}
		}
	}
	// a wayland session with only xsel and no DISPLAY has no way
	d := xdgDesktop(DesktopEnv{LookPath: lookIn("xsel", "xclip"), Getenv: envOf("WAYLAND_DISPLAY", "w")})
	if d.Clipboard != nil {
		t.Error("an X11 tool was offered with no X display")
	}
}

// xclip and wl-copy stay in the background to serve the clipboard; the runner gives up waiting for them with
// ErrWaitDelay, which is a success for the clipboard.
func TestXDGClipboardToleratesTheBackgroundCopy(t *testing.T) {
	p := &progRun{err: map[string]error{"xclip": exec.ErrWaitDelay}}
	d := xdgDesktop(DesktopEnv{Run: p.run, LookPath: lookIn("xclip"), Getenv: envOf("DISPLAY", ":0")})
	if err := d.Clipboard.Copy(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	p.err["xclip"] = errors.New("boom")
	if err := d.Clipboard.Copy(context.Background(), "x"); err == nil {
		t.Fatal("a failure of xclip was lost")
	}
}

func TestZenityArgvTable(t *testing.T) {
	const title = "--title=Rubi Home"
	base := []string{"--no-markup", "--width=460", title}
	for _, c := range []struct {
		buttons []string
		want    []string
	}{
		{[]string{"OK"}, append(append([]string{}, base...), "--text=T", "--info", "--ok-label=OK")},
		{[]string{"Close", "Copy"}, append(append([]string{}, base...), "--text=T", "--question", "--ok-label=Copy", "--cancel-label=Close")},
		{[]string{"Close", "Copy", "Open"}, append(append([]string{}, base...), "--text=T", "--question", "--ok-label=Open", "--cancel-label=Close", "--extra-button=Copy")},
	} {
		if got := zenityArgs("T", c.buttons); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v:\n got %q\nwant %q", c.buttons, got, c.want)
		}
	}
}

func TestZenityDialogAnswers(t *testing.T) {
	b3 := []string{"Close", "Copy", "Open"}
	one := exitErr(t, 1)
	for _, c := range []struct {
		name    string
		buttons []string
		out     string
		err     error
		want    string
		fail    bool
	}{
		{"OK is the last", b3, "", nil, "Open", false},
		{"Cancel is the first", b3, "", one, "Close", false},
		{"the extra button is printed", b3, "Copy\n", one, "Copy", false},
		{"an unknown printed label is Close", b3, "Evil\n", one, "Close", false},
		{"two buttons, OK", []string{"Close", "Copy"}, "", nil, "Copy", false},
		{"two buttons, the close box", []string{"Close", "Copy"}, "", one, "Close", false},
		{"one button", []string{"OK"}, "", nil, "OK", false},
		{"a failure is an error", b3, "", exitErr(t, 5), "", true},
		{"a missing program is an error", b3, "", exec.ErrNotFound, "", true},
	} {
		got, err := zenityAnswer(c.buttons, c.out, c.err)
		if (err != nil) != c.fail || got != c.want {
			t.Errorf("%s: %q %v", c.name, got, err)
		}
	}
}

// zw is what kdialog's text starts with, so that Qt does not read it as markup.
const zw = "\u200b"

func TestKdialogArgvTable(t *testing.T) {
	for _, c := range []struct {
		buttons []string
		want    []string
	}{
		{[]string{"OK"}, []string{"--title", "Rubi Home", "--msgbox", zw + "T"}},
		{[]string{"Close", "Copy"}, []string{"--title", "Rubi Home", "--yes-label", "Copy", "--no-label", "Close", "--yesno", zw + "T"}},
		{[]string{"Close", "Copy", "Open"}, []string{"--title", "Rubi Home", "--yes-label", "Open", "--no-label", "Copy", "--cancel-label", "Close", "--yesnocancel", zw + "T"}},
	} {
		if got := kdialogArgs("T", c.buttons); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v:\n got %q\nwant %q", c.buttons, got, c.want)
		}
	}
	b3 := []string{"Close", "Copy", "Open"}
	for _, c := range []struct {
		buttons []string
		err     error
		want    string
	}{
		{b3, nil, "Open"}, {b3, exitErr(t, 1), "Copy"}, {b3, exitErr(t, 2), "Close"},
		{[]string{"Close", "Copy"}, nil, "Copy"}, {[]string{"Close", "Copy"}, exitErr(t, 1), "Close"},
	} {
		if got, err := kdialogAnswer(c.buttons, "", c.err); err != nil || got != c.want {
			t.Errorf("%v %v: %q %v", c.buttons, c.err, got, err)
		}
	}
	if _, err := kdialogAnswer(b3, "", exitErr(t, 4)); err == nil {
		t.Error("kdialog failing was taken for a button")
	}
}

// The text goes to the program as one argument that cannot be an option, whatever it says; nothing is run through a
// shell.
func TestXDGDialogNeverMakesTextAnOptionOrAShellLine(t *testing.T) {
	hostile := `-- --exec=rm; $(id) "quoted" 'single' ` + "`tick`" + ` <b>markup</b> %s \n`
	for _, tool := range []string{"zenity", "kdialog"} {
		p := &progRun{}
		d := xdgDesktop(DesktopEnv{Run: p.run, LookPath: lookIn(tool), Getenv: envOf("DISPLAY", ":0")})
		if _, err := d.Dialogs.Message(context.Background(), hostile, "Close", "Copy", "Open"); err != nil {
			t.Fatal(err)
		}
		if len(p.calls) != 1 || p.calls[0].name != tool || p.calls[0].stdin != "" {
			t.Fatalf("%s: %+v", tool, p.calls)
		}
		found := 0
		for i, a := range p.calls[0].args {
			if strings.Contains(a, "$(id)") {
				found++
				// zenity: joined to its option; kdialog: the argument right after its option
				if tool == "zenity" && !strings.HasPrefix(a, "--text=") {
					t.Errorf("zenity: the text is not joined to --text=: %q", a)
				}
				if tool == "kdialog" && !strings.HasPrefix(p.calls[0].args[i-1], "--") {
					t.Errorf("kdialog: the text does not follow an option: %q", p.calls[0].args[i-1])
				}
			}
		}
		if found != 1 {
			t.Errorf("%s: the text appears %d times in argv %q", tool, found, p.calls[0].args)
		}
		if tool == "zenity" && !contains(p.calls[0].args, "--no-markup") {
			t.Error("zenity would read the text as markup")
		}
	}
	// zenity before kdialog when both exist; one to three buttons only
	p := &progRun{}
	d := xdgDesktop(DesktopEnv{Run: p.run, LookPath: lookIn("kdialog", "zenity"), Getenv: envOf("DISPLAY", ":0")})
	_, _ = d.Dialogs.Message(context.Background(), "t", "OK")
	if p.calls[0].name != "zenity" {
		t.Errorf("preferred %s", p.calls[0].name)
	}
	if _, err := d.Dialogs.Message(context.Background(), "t"); err == nil {
		t.Error("a dialog with no button")
	}
	if _, err := d.Dialogs.Message(context.Background(), "t", "a", "b", "c", "d"); err == nil {
		t.Error("a dialog with four buttons")
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestXDGRefusesALinkThatIsNotOurs(t *testing.T) {
	p := &progRun{}
	d := xdgDesktop(DesktopEnv{Run: p.run, LookPath: lookIn("xdg-open"), Getenv: envOf("DISPLAY", ":0")})
	for _, bad := range []string{"", "file:///etc/passwd", "javascript:alert(1)", "-n", "--help", "https://x/ y", "https://x/\"y", "https://x/\ny",
		"https://x/`id`", "ftp://x/y", "https:///nohost", "https://x/a|b", "mailto:a@b"} {
		if err := d.Opener.Open(context.Background(), bad); err == nil {
			t.Errorf("%q was opened", bad)
		}
	}
	if len(p.calls) != 0 {
		t.Fatalf("ran for a refused link: %+v", p.calls)
	}
	for _, ok := range []string{"https://rubi-panel.com/#home-code=a%2Bb", "claude://code/new?folder=%2Fp&q=hi%20there", "http://localhost:3000/x?a=1&b=2"} {
		if err := d.Opener.Open(context.Background(), ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}

// Qt reads a text that starts with a tag as rich text; the text kdialog gets never does, whatever it says.
func TestKdialogTextCannotBeRichText(t *testing.T) {
	for _, text := range []string{"<b>bold</b>", "<html><img src=x></html>", "  <a href='http://x'>x</a>", "plain"} {
		a := kdialogArgs(text, []string{"OK"})
		shown := a[len(a)-1]
		if !strings.HasPrefix(shown, noRichText) || strings.HasPrefix(strings.TrimSpace(shown), "<") || !strings.HasSuffix(shown, text) {
			t.Errorf("%q is shown as %q", text, shown)
		}
	}
}
