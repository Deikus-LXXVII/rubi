//go:build !linux

package agentsandbox

import (
	"context"
	"errors"
	"os/exec"
)

// Repository task admission refuses these platforms. Preserve standalone srt
// behavior; this fallback is not a claim of task-wide descendant supervision.
func runConfined(ctx context.Context, cmd *exec.Cmd, ownTree bool) error {
	if ownTree {
		return errors.New("sandbox: supervised process-tree ownership requires Linux")
	}
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	return cmd.Run()
}
