package home

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
)

// PanelURL is the Rubi panel's address; the pairing link adds "#home-code=<code>" to it.
const PanelURL = "https://rubi-panel.com/"

const (
	btnOpenPanel = "Open the Rubi panel"
	btnCopy      = "Copy the code"
	btnCopyAgain = "Copy the code again"
	btnClose     = "Close"
	btnOK        = "OK"
	btnPairAgain = "Pair again"

	dialogTitle = "Rubi Home"
	pairText    = "Pair this computer with your Rubi. The code works once, for 15 minutes."
)

// Fingerprint is the short fingerprint of this Home's identity; the Rubi panel shows the same when pairing.
func (h *Helper) Fingerprint() string {
	sum := sha256.Sum256([]byte(h.ID.PublicBundle()))
	return base64.RawURLEncoding.EncodeToString(sum[:6])
}

// PairingLink is the panel address that carries the code in its fragment (never sent to a server).
func PairingLink(code string) string {
	return PanelURL + "#home-code=" + url.QueryEscape(code)
}

// RunFunc runs a program; stdin is sent to it when not empty. It returns the program's standard output.
type RunFunc func(ctx context.Context, stdin string, name string, args ...string) (string, error)

// OpenEnv is what the open window talks to; tests replace it.
type OpenEnv struct {
	Run      RunFunc
	Out      io.Writer // where the text mode prints
	Desktop  *Desktop  // the windows, the browser and the clipboard; NewDesktop of this system when nil
	LookPath func(string) (string, error)
	Getenv   func(string) string
}

func execRun(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	// A program that keeps a copy of the clipboard in the background (xclip, wl-copy) holds our pipe open; after
	// the program itself ends we wait a second for it and then let go.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	return string(out), err
}

func (env OpenEnv) desktop() Desktop {
	if env.Desktop != nil {
		return *env.Desktop
	}
	return NewDesktop(DesktopEnv{Run: env.Run, LookPath: env.LookPath, Getenv: env.Getenv})
}

// who names this computer the way the Rubi panel shows it: the user compares the two.
func (h *Helper) who() string {
	return fmt.Sprintf("Computer: %s\nFingerprint: %s", h.Config().Name, h.Fingerprint())
}

// RunOpen is `rubi-home open`: the window the user sees when they open Rubi Home. A pairing code is made only
// when the user presses Copy or Open (a still-good pending one is reused, not replaced), so Close leaves
// nothing live behind. The pending secret goes in the config, which a running helper reloads.
//
// Where the computer has a desktop with a way to show a window (macOS; Linux with zenity or kdialog; Windows) the
// window shows the PIN and offers to copy the code and to open the panel. Where it has none, the same things are
// printed (the link, the PIN and the code) for whoever runs Rubi Home, a Bot included, to pass on.
func RunOpen(ctx context.Context, h *Helper, env OpenEnv) error {
	d := env.desktop()
	if d.Dialogs == nil || (d.Opener == nil && d.Clipboard == nil) {
		return printPairing(h, env.Out)
	}
	if cfg := h.Fresh(); len(cfg.Paired) > 0 {
		p := cfg.Paired[len(cfg.Paired)-1]
		label := strings.TrimSpace(p.Label)
		if label == "" {
			label = "Rubi"
		}
		text := fmt.Sprintf("Paired with %s since %s. Fingerprint %s (the Rubi panel shows the same when you pair).",
			label, p.At.Local().Format("2 January 2006"), h.Fingerprint())
		c, err := d.Dialogs.Message(ctx, text, btnClose, btnPairAgain)
		if errors.Is(err, ErrNoDesktop) {
			return printPairing(h, env.Out)
		}
		if err != nil {
			return windowFailed(env.Out, err)
		}
		if c != btnPairAgain {
			return nil
		}
	}
	return pairFlow(ctx, h, env.Out, d)
}

// windowFailed is what a window that failed for another reason than "there is no desktop" says: the error, and the way to
// pair from the panel. It prints no code and makes none: a failure is not a reason to put a secret on a screen or in a log.
func windowFailed(out io.Writer, err error) error {
	fmt.Fprintln(out, "Rubi Home couldn't show its window. To pair this computer, open the Rubi panel ("+PanelURL+"),")
	fmt.Fprintln(out, "go to Settings, Rubi Home, and ask for a pairing code from Rubi Home's window once it opens, or try again.")
	return err
}

// printPairing is the text mode: it makes a code and prints what the window would show.
func printPairing(h *Helper, out io.Writer) error {
	code, pin, err := h.PairingCode(true)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "Pair this computer with your Rubi. The code works once, for 15 minutes.")
	fmt.Fprintln(out, h.who())
	fmt.Fprintln(out, "Open this link in a browser, then type the PIN "+homeproto.FormatPIN(pin)+" in the Rubi panel:")
	fmt.Fprintln(out, PairingLink(code.WithoutSecret().String()))
	fmt.Fprintln(out, "Or paste this code in the Rubi panel (Settings, Rubi Home, Add a home computer), with no PIN:")
	fmt.Fprintln(out, code.String())
	return nil
}

func pairFlow(ctx context.Context, h *Helper, out io.Writer, d Desktop) error {
	buttons := []string{btnClose}
	if d.Clipboard != nil {
		buttons = append(buttons, btnCopy)
	}
	if d.Opener != nil {
		buttons = append(buttons, btnOpenPanel)
	}
	c, err := d.Dialogs.Message(ctx, pairText+"\n\n"+h.who(), buttons...)
	if errors.Is(err, ErrNoDesktop) {
		return printPairing(h, out)
	}
	if err != nil {
		return windowFailed(out, err)
	}
	var code homeproto.Code
	var pin, follow string
	for err == nil && c != btnClose && c != btnOK {
		if code.Relay == "" {
			if code, pin, err = h.PairingCode(true); err != nil {
				return err
			}
		}
		switch c {
		case btnOpenPanel: // the link carries no secret: the PIN in the next dialog is typed in the panel
			err = d.Opener.Open(ctx, PairingLink(code.WithoutSecret().String()))
			follow = "Open Rubi from your own link, then Settings, Rubi Home, \"Pair the waiting Rubi Home\", type the PIN, approve with your passkey." +
				"\n\nPIN: " + homeproto.FormatPIN(pin)
		case btnCopy, btnCopyAgain:
			err = d.Clipboard.Copy(ctx, code.String()) // on stdin, never in argv
			if follow == "" {
				follow = "The code is on your clipboard. In the Rubi panel: Settings, Rubi Home, Add a home computer, paste it, then Pair. Approve with your passkey."
			}
		default:
			return nil
		}
		if err != nil {
			break
		}
		next := []string{btnOK}
		if d.Clipboard != nil {
			next = append(next, btnCopyAgain)
		}
		c, err = d.Dialogs.Message(ctx, follow+"\n\n"+h.who()+"\n(it must match the Rubi panel)", next...)
	}
	return err
}
