package core

import "time"

// SetSeenGraceForTests shortens the wait after an approval is decided (see seenGrace). Call it before any
// Core is opened, from a test's TestMain; the product never does.
func SetSeenGraceForTests(d time.Duration) { seenGrace = d }
