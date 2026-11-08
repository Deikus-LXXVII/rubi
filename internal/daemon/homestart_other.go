//go:build !linux

package daemon

import "context"

// keepHome does nothing here: macOS and Windows start Rubi Home at login themselves.
func keepHome(context.Context) {}
