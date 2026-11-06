package home

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// schemeName is a link scheme as asked about: lower-case letters, digits and dashes, so it can't carry anything else into
// an argument or a registry path.
var schemeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// xdgDesktop is the Linux form of Desktop: xdg-open for links, wl-copy, xclip or xsel for the clipboard, zenity or
// kdialog for windows. All of it needs a graphical session (DISPLAY or WAYLAND_DISPLAY); a box without one, or
// without the tools, gets the parts it can have, and the pairing window prints its text instead.
func xdgDesktop(env DesktopEnv) Desktop {
	env = env.withDefaults()
	wayland, x11 := env.Getenv("WAYLAND_DISPLAY") != "", env.Getenv("DISPLAY") != ""
	if !wayland && !x11 {
		return Desktop{}
	}
	has := func(name string) bool { _, err := env.LookPath(name); return err == nil }
	var d Desktop
	if has("xdg-open") {
		d.Opener = openerFunc(func(ctx context.Context, link string) error {
			if err := checkLink(link); err != nil {
				return err
			}
			_, err := env.Run(ctx, "", "xdg-open", link)
			return err
		})
	}
	if has("xdg-mime") {
		// the program registered for x-scheme-handler/<scheme> is named on the first line; nothing registered prints nothing
		d.Scheme = schemeFunc(func(ctx context.Context, scheme string) bool {
			if !schemeName.MatchString(scheme) {
				return false
			}
			out, err := env.Run(ctx, "", "xdg-mime", "query", "default", "x-scheme-handler/"+scheme)
			return err == nil && strings.TrimSpace(out) != ""
		})
	}
	var copyArgv []string
	switch {
	case wayland && has("wl-copy"):
		copyArgv = []string{"wl-copy"}
	case x11 && has("xclip"):
		copyArgv = []string{"xclip", "-selection", "clipboard", "-in"}
	case x11 && has("xsel"):
		copyArgv = []string{"xsel", "--clipboard", "--input"}
	}
	if copyArgv != nil {
		d.Clipboard = clipboardFunc(func(ctx context.Context, text string) error {
			// These tools stay in the background to serve the clipboard; the runner's wait delay lets go of them.
			_, err := env.Run(ctx, text, copyArgv[0], copyArgv[1:]...)
			if errors.Is(err, exec.ErrWaitDelay) {
				return nil
			}
			return err
		})
	}
	switch {
	case has("zenity"):
		d.Dialogs = dialogsFunc(func(ctx context.Context, text string, buttons ...string) (string, error) {
			if err := checkButtons(buttons); err != nil {
				return "", err
			}
			out, err := env.Run(ctx, "", "zenity", zenityArgs(text, buttons)...)
			return zenityAnswer(buttons, out, err)
		})
	case has("kdialog"):
		d.Dialogs = dialogsFunc(func(ctx context.Context, text string, buttons ...string) (string, error) {
			if err := checkButtons(buttons); err != nil {
				return "", err
			}
			out, err := env.Run(ctx, "", "kdialog", kdialogArgs(text, buttons)...)
			return kdialogAnswer(buttons, out, err)
		})
	}
	return d
}

// zenityArgs: the text and every label are joined to their option with "=", so none can be taken for an option;
// --no-markup makes the text plain text, not Pango markup. With three buttons, the last is OK (the default), the
// first is Cancel (Escape, the window's close box) and the middle one is an extra button, which zenity prints.
func zenityArgs(text string, buttons []string) []string {
	a := []string{"--no-markup", "--width=460", "--title=" + dialogTitle, "--text=" + text}
	switch len(buttons) {
	case 1:
		return append(a, "--info", "--ok-label="+buttons[0])
	case 2:
		return append(a, "--question", "--ok-label="+buttons[1], "--cancel-label="+buttons[0])
	}
	return append(a, "--question", "--ok-label="+buttons[2], "--cancel-label="+buttons[0], "--extra-button="+buttons[1])
}

func zenityAnswer(buttons []string, out string, err error) (string, error) {
	if err == nil {
		return buttons[len(buttons)-1], nil // OK
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		// Cancel, the close box, Escape: the first button; an extra button is printed on standard output.
		if s := strings.TrimSpace(out); len(buttons) == 3 && s == buttons[1] {
			return s, nil
		}
		return buttons[0], nil
	}
	return "", fmt.Errorf("dialog: %w", err)
}

// noRichText starts every text kdialog shows: Qt takes a text for rich text (markup) when it starts with a tag, and a
// zero-width space is not a tag, nor white space that Qt skips.
const noRichText = "\u200b"

// kdialogArgs: the text is the argument of its option. With three buttons, yes is the last, no the middle and cancel
// the first.
func kdialogArgs(text string, buttons []string) []string {
	text = noRichText + text
	a := []string{"--title", dialogTitle}
	switch len(buttons) {
	case 1:
		return append(a, "--msgbox", text)
	case 2:
		return append(a, "--yes-label", buttons[1], "--no-label", buttons[0], "--yesno", text)
	}
	return append(a, "--yes-label", buttons[2], "--no-label", buttons[1], "--cancel-label", buttons[0], "--yesnocancel", text)
}

func kdialogAnswer(buttons []string, _ string, err error) (string, error) {
	if err == nil {
		return buttons[len(buttons)-1], nil // yes, or OK
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case 1: // no
			if len(buttons) == 3 {
				return buttons[1], nil
			}
			return buttons[0], nil
		case 2: // cancel
			return buttons[0], nil
		}
	}
	return "", fmt.Errorf("dialog: %w", err)
}
