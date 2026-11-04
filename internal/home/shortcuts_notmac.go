//go:build !darwin

package home

import "context"

// runShortcuts on a system without the Shortcuts app: nothing is run; the answer is the same not_available_here that
// Handle gives before it gets here (features.go), in case a caller reaches it another way.
func runShortcuts(context.Context, ...string) ([]byte, error) {
	return nil, notAvailable(Feature{Reason: "it needs macOS"})
}
