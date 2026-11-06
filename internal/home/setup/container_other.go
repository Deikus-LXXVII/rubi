//go:build !darwin

package setup

import "context"

// verifyPackage is the macOS check of Apple's installer package (container_darwin.go); the package is installed on a Mac only.
func verifyPackage(context.Context, Commander, string, string) error { return errNotMac }
