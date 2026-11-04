package home

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// What Rubi Home needs from the desktop it runs on: open a link, put a code on the clipboard, and ask a question in
// a window. Each is a small interface with one form per system (docs/plans/home-platforms.md, stage P3):
//
//	macOS    desktop_darwin.go   open, pbcopy, osascript
//	Linux    desktop_xdg.go      xdg-open, wl-copy or xclip or xsel, zenity or kdialog
//	Windows  desktop_winps.go    rundll32, clip.exe, a PowerShell window
//
// Every form runs a program with an argument list and nothing else: no shell, no AppleScript or PowerShell
// source with text spliced in, no `cmd /c`. A text the user may see or a link travels as one argument or on the
// program's standard input; the only script text is a constant. A part that a computer does not have is nil in
// Desktop, and the code that needs it falls back (the pairing window prints the link, the PIN and the code).

// ErrNoDesktop says a window can not be shown here (no display, no tool for it).
var ErrNoDesktop = errors.New("this computer has no desktop to show a window on")

// Opener opens a link in the user's browser or in the app that owns its scheme.
type Opener interface {
	Open(ctx context.Context, link string) error
}

// Clipboard puts a text on the clipboard. The text goes to the program on its standard input, never in argv.
type Clipboard interface {
	Copy(ctx context.Context, text string) error
}

// Dialogs shows a message with one to three buttons and returns the label of the one pressed. The last button is
// the default; closing the window, or Escape, counts as the first, which is always the harmless one.
type Dialogs interface {
	Message(ctx context.Context, text string, buttons ...string) (string, error)
}

// Desktop is what this computer can do; a nil part is not available here.
type Desktop struct {
	Opener    Opener
	Clipboard Clipboard
	Dialogs   Dialogs
}

// DesktopEnv is what a Desktop is made from; tests replace every part.
type DesktopEnv struct {
	Run      RunFunc
	LookPath func(string) (string, error)
	Getenv   func(string) string
	// SystemDir is Windows' System32 folder as the system names it (not from PATH); the Windows form looks for its
	// programs there and nowhere else.
	SystemDir string
}

func (e DesktopEnv) withDefaults() DesktopEnv {
	if e.Run == nil {
		e.Run = execRun
	}
	if e.LookPath == nil {
		e.LookPath = exec.LookPath
	}
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	return e
}

// NewDesktop is the Desktop of the system Rubi Home is running on.
func NewDesktop(env DesktopEnv) Desktop { return platformDesktop(env.withDefaults()) }

// The adapters that let a form of a system be written as three functions.
type (
	openerFunc    func(ctx context.Context, link string) error
	clipboardFunc func(ctx context.Context, text string) error
	dialogsFunc   func(ctx context.Context, text string, buttons ...string) (string, error)
)

func (f openerFunc) Open(ctx context.Context, link string) error { return f(ctx, link) }
func (f clipboardFunc) Copy(ctx context.Context, text string) error {
	return f(ctx, text)
}
func (f dialogsFunc) Message(ctx context.Context, text string, buttons ...string) (string, error) {
	return f(ctx, text, buttons...)
}

// checkLink refuses anything but an http, https or claude link without spaces, control characters or quotes: what
// is opened is a link Rubi made, and a program that takes it as an argument must not see anything else in it.
func checkLink(link string) error {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "claude") || u.Host == "" {
		return errors.New("not a link Rubi Home opens")
	}
	if strings.ContainsFunc(link, func(r rune) bool {
		return r <= ' ' || r == 0x7f || r == '"' || r == '\'' || r == '`' || r == '\\' || r == '^' || r == '|' || r == '<' || r == '>'
	}) {
		return errors.New("not a link Rubi Home opens")
	}
	return nil
}

// checkButtons refuses a dialog with no button or more than three.
func checkButtons(buttons []string) error {
	if len(buttons) < 1 || len(buttons) > 3 {
		return errors.New("a dialog has one to three buttons")
	}
	return nil
}

// pick returns the button whose label is s, else the first.
func pick(buttons []string, s string) string {
	s = strings.TrimSpace(s)
	for _, b := range buttons {
		if b == s {
			return b
		}
	}
	return buttons[0]
}
