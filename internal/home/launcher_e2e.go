//go:build rubie2e

package home

// Only in the end-to-end test build (go build -tags rubie2e), never in a release: the test can't open the
// desktop app, so a launch appends its URL to the file named by RUBI_E2E_LAUNCH_FILE and the test driver
// plays the SessionStart hook of the session that would have started.

import (
	"context"
	"os"
)

type fileLauncher struct{ path string }

func (f fileLauncher) Launch(_ context.Context, cwd, prompt string) error {
	fh, err := os.OpenFile(f.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer fh.Close()
	_, err = fh.WriteString(desktopURL(cwd, prompt) + "\n")
	return err
}

func init() {
	if p := os.Getenv("RUBI_E2E_LAUNCH_FILE"); p != "" {
		newDefaultLauncher = func() Launcher { return fileLauncher{p} }
	}
}
