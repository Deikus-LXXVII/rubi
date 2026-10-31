package mcpserver_test

import (
	"os"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/core"
	"github.com/Deikus-LXXVII/rubi/internal/panelclient"
)

// seenGrace is Rubi's wait for an agent to pick up an approval's outcome, shortened for the tests;
// "past the grace period" waits are twice it plus slack (pastGrace).
const (
	seenGrace = 250 * time.Millisecond
	pastGrace = 1500 * time.Millisecond
)

// The tests unlock with a password many times; Argon2id under the race detector is the slowest thing here.
func TestMain(m *testing.M) {
	panelclient.MemoizeKDFForTests()
	core.SetSeenGraceForTests(seenGrace)
	os.Exit(m.Run())
}
