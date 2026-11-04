package home

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runShortcuts runs the macOS shortcuts command (Shortcuts.app's command line).
func runShortcuts(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "shortcuts", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return out, nil
}
