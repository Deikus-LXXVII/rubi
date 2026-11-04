package home

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// pkexecPath is pkexec by its absolute path: never the first one on a PATH the user can write to.
const pkexecPath = "/usr/bin/pkexec"

// pkexecElevator is the Linux form: `/usr/bin/pkexec /bin/sh -c SCRIPT`. polkit asks for the administrator password through
// the authentication agent of the user's desktop session; there is no terminal prompt. The script is one argument (15 KB with
// the plugin; a single argument may be 128 KB on Linux) that only /bin/sh reads; pkexec resets the environment itself.
//
// Without a graphical session, or without pkexec, nobody can be asked: Available says ErrNoAgent and the installation
// goes the headless way (the Bot's sudo, after the panel approval). A session that looks graphical may still have no polkit
// agent; pkexec then says so and Home remembers it for a few minutes (hookNoAgentFor). There is no reliable way to ask
// polkit beforehand whether an agent is registered for this session (the authority has no such question), so the answer
// comes from trying. pkexec cannot be given a message of ours (that needs a policy file, which needs root to install), so
// polkit's own words are shown; the approval says what the prompt is for.
func pkexecElevator(env ElevateEnv) Elevator {
	const sh = "/bin/sh"
	return elevatorFuncs{
		describe: pkexecPath + " /bin/sh -c SCRIPT (polkit asks for an administrator password in your desktop session)",
		sentence: "Your desktop will ask for the administrator password on that computer (polkit); approve only if you asked for this",
		available: func(context.Context) error {
			if env.Geteuid() == 0 {
				return nil
			}
			if _, err := env.LookPath(pkexecPath); err != nil {
				return fmt.Errorf("%w: pkexec is not installed", ErrNoAgent)
			}
			if !hasDesktopSession(env.Getenv) {
				return fmt.Errorf("%w: there is no desktop session", ErrNoAgent)
			}
			return nil
		},
		run: func(ctx context.Context, p InstallPlan) error {
			if _, err := env.LookPath(pkexecPath); err != nil {
				return fmt.Errorf("%w: pkexec is not installed", ErrNoAgent)
			}
			out, err := env.Exec(ctx, privilegedEnv(), pkexecPath, sh, "-c", p.FullScript())
			if err == nil {
				return nil
			}
			return pkexecResult(out, err)
		},
	}
}

// hasDesktopSession says whether the user is in a graphical session: the session type systemd's logind sets, or a display.
func hasDesktopSession(getenv func(string) string) bool {
	switch strings.ToLower(getenv("XDG_SESSION_TYPE")) {
	case "x11", "wayland", "mir":
		return true
	}
	return getenv("DISPLAY") != "" || getenv("WAYLAND_DISPLAY") != ""
}

// pkexecResult maps pkexec's way of saying no. Its exit status 126 is "the dialog was dismissed" and 127 "not authorized,
// or an error", but the script's own failures can end in 126 or 127 too, and the script's output is in the same stream. So
// only the lines that pkexec itself writes count: they start with "Error executing command as another user:" (or "Error
// getting authority:"), which no line of the script's does.
func pkexecResult(out []byte, err error) error {
	for _, line := range strings.Split(string(out), "\n") {
		var msg string
		switch {
		case strings.HasPrefix(line, "Error executing command as another user:"):
			msg = strings.TrimPrefix(line, "Error executing command as another user:")
		case strings.HasPrefix(line, "Error getting authority:"):
			msg = strings.TrimPrefix(line, "Error getting authority:")
		default:
			continue
		}
		switch {
		case strings.Contains(msg, "No authentication agent found"):
			return ErrNoAgent
		case strings.Contains(msg, "Request dismissed"), strings.Contains(msg, "Not authorized"):
			return ErrAdminCancelled
		}
	}
	return errors.New("pkexec: " + trimOut(out, err))
}
