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
	"runtime"
	"strings"

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
	Run  RunFunc
	Out  io.Writer // where the non-macOS fallback prints
	GOOS string    // runtime.GOOS when empty
}

func execRun(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.Output()
	return string(out), err
}

// who names this computer the way the Rubi panel shows it: the user compares the two.
func (h *Helper) who() string {
	return fmt.Sprintf("Computer: %s\nFingerprint: %s", h.Config().Name, h.Fingerprint())
}

// RunOpen is `rubi-home open`: the window the user sees when they open Rubi Home. A pairing code is made only
// when the user presses Copy or Open (a still-good pending one is reused, not replaced), so Close leaves
// nothing live behind. The pending secret goes in the config, which a running helper reloads.
func RunOpen(ctx context.Context, h *Helper, env OpenEnv) error {
	if env.Run == nil {
		env.Run = execRun
	}
	if env.GOOS == "" {
		env.GOOS = runtime.GOOS
	}
	if env.GOOS != "darwin" {
		code, pin, err := h.PairingCode(true)
		if err != nil {
			return err
		}
		fmt.Fprintln(env.Out, "Pair this computer with your Rubi. The code works once, for 15 minutes.")
		fmt.Fprintln(env.Out, h.who())
		fmt.Fprintln(env.Out, "Open this link in a browser, then type the PIN "+homeproto.FormatPIN(pin)+" in the Rubi panel:")
		fmt.Fprintln(env.Out, PairingLink(code.WithoutSecret().String()))
		fmt.Fprintln(env.Out, "Or paste this code in the Rubi panel (Settings, Rubi Home, Add a home computer), with no PIN:")
		fmt.Fprintln(env.Out, code.String())
		return nil
	}
	if cfg := h.Fresh(); len(cfg.Paired) > 0 {
		p := cfg.Paired[len(cfg.Paired)-1]
		label := strings.TrimSpace(p.Label)
		if label == "" {
			label = "Rubi"
		}
		text := fmt.Sprintf("Paired with %s since %s. Fingerprint %s (the Rubi panel shows the same when you pair).",
			label, p.At.Local().Format("2 January 2006"), h.Fingerprint())
		c, err := dialog(ctx, env, text, btnClose, btnPairAgain)
		if err != nil || c != btnPairAgain {
			return err
		}
	}
	return pairFlow(ctx, h, env)
}

func pairFlow(ctx context.Context, h *Helper, env OpenEnv) error {
	c, err := dialog(ctx, env, pairText+"\n\n"+h.who(), btnClose, btnCopy, btnOpenPanel)
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
			_, err = env.Run(ctx, "", "/usr/bin/open", PairingLink(code.WithoutSecret().String()))
			follow = "Open Rubi from your own link, then Settings, Rubi Home, \"Pair the waiting Rubi Home\", type the PIN, approve with your passkey." +
				"\n\nPIN: " + homeproto.FormatPIN(pin)
		case btnCopy, btnCopyAgain:
			_, err = env.Run(ctx, code.String(), "/usr/bin/pbcopy") // on stdin, never in argv
			if follow == "" {
				follow = "The code is on your clipboard. In the Rubi panel: Settings, Rubi Home, Add a home computer, paste it, then Pair. Approve with your passkey."
			}
		default:
			return nil
		}
		if err != nil {
			break
		}
		c, err = dialog(ctx, env, follow+"\n\n"+h.who()+"\n(it must match the Rubi panel)", btnOK, btnCopyAgain)
	}
	return err
}

// dialogArgs is osascript's argv. Every text travels as an argument of the run handler, so AppleScript
// never has to quote it. The last button is the default.
func dialogArgs(text string, buttons ...string) []string {
	a := []string{"-e", "on run argv",
		"-e", "set bs to items 3 thru -1 of argv",
		"-e", "set r to display dialog (item 1 of argv) with title (item 2 of argv) buttons bs default button (last item of bs)",
		"-e", "return button returned of r",
		"-e", "end run", text, dialogTitle}
	return append(a, buttons...)
}

// dialog shows text with up to three buttons (the last is the default) and returns the one pressed;
// Escape counts as the first button, which is always the harmless one.
func dialog(ctx context.Context, env OpenEnv, text string, buttons ...string) (string, error) {
	out, err := env.Run(ctx, "", "/usr/bin/osascript", dialogArgs(text, buttons...)...)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "(-128)") {
			return buttons[0], nil
		}
		if strings.Contains(err.Error(), "(-128)") {
			return buttons[0], nil
		}
		return "", fmt.Errorf("dialog: %w", err)
	}
	pressed := strings.TrimSpace(out)
	for _, b := range buttons {
		if b == pressed {
			return b, nil
		}
	}
	return buttons[0], nil
}
