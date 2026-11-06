package home

import (
	"context"
	"fmt"
	"strings"
)

// adminPrompt names the request in the macOS administrator dialog itself.
const adminPrompt = "Rubi Home wants to install the Claude Code hook (approved in your Rubi panel)."

// osascriptArgs is the argv of the administrator run. The script and the prompt travel as arguments of the
// run handler, so AppleScript never has to quote them.
func osascriptArgs(p InstallPlan) []string {
	return []string{"-e", "on run argv",
		"-e", "do shell script (item 1 of argv) with administrator privileges with prompt (item 2 of argv)",
		"-e", "end run", p.FullScript(), adminPrompt}
}

// platformElevator is the macOS form: the administrator dialog of osascript, as it always was.
func platformElevator(env ElevateEnv) Elevator {
	return elevatorFuncs{
		describe: "/usr/bin/osascript, \"do shell script ... with administrator privileges\" (macOS asks for an administrator password)",
		sentence: "macOS will ask for the administrator password on that computer; approve only if you asked for this",
		run: func(ctx context.Context, p InstallPlan) error {
			out, err := env.Exec(ctx, privilegedEnv(), "/usr/bin/osascript", osascriptArgs(p)...)
			if err != nil {
				if strings.Contains(string(out), "(-128)") || strings.Contains(string(out), "User canceled") {
					return ErrAdminCancelled
				}
				return fmt.Errorf("osascript: %v: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		},
	}
}
