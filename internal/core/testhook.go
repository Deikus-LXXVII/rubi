package core

import (
	"testing"
	"time"
)

// SetSeenGraceForTests shortens the wait after an approval is decided (see seenGrace). Call it before any
// Core is opened, from a test's TestMain. It does nothing outside a test binary, so the product can't use it.
func SetSeenGraceForTests(d time.Duration) {
	if testing.Testing() {
		seenGrace = d
	}
}
