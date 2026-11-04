package home

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// platformDesktop is the macOS form: open, pbcopy and an osascript dialog.
func platformDesktop(env DesktopEnv) Desktop {
	return Desktop{
		Opener: openerFunc(func(ctx context.Context, link string) error {
			if err := checkLink(link); err != nil {
				return err
			}
			_, err := env.Run(ctx, "", "/usr/bin/open", link)
			return err
		}),
		Clipboard: clipboardFunc(func(ctx context.Context, text string) error {
			_, err := env.Run(ctx, text, "/usr/bin/pbcopy") // on stdin, never in argv
			return err
		}),
		// Every text travels as an argument of the run handler, so AppleScript never has to quote it. After "--" (so
		// that a text that starts with a dash is not an option) the first two arguments are the text and the title, the rest
		// are the buttons.
		Dialogs: dialogsFunc(func(ctx context.Context, text string, buttons ...string) (string, error) {
			if err := checkButtons(buttons); err != nil {
				return "", err
			}
			args := []string{"-e", "on run argv",
				"-e", "set bs to items 3 thru -1 of argv",
				"-e", "set r to display dialog (item 1 of argv) with title (item 2 of argv) buttons bs default button (last item of bs)",
				"-e", "return button returned of r",
				"-e", "end run", "--", text, dialogTitle}
			out, err := env.Run(ctx, "", "/usr/bin/osascript", append(args, buttons...)...)
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
			return pick(buttons, out), nil
		}),
	}
}
