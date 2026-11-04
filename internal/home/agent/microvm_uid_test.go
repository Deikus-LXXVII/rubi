package agent

import (
	"strconv"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/microvm"
)

// The microvm's virtiofsd maps the runner's user to the person's own: it must be the user the runner image runs as.
func TestMicrovmGuestUIDIsTheRunners(t *testing.T) {
	if strconv.Itoa(microvm.GuestUID) != runnerUID {
		t.Fatalf("microvm.GuestUID %d, the runner's %s", microvm.GuestUID, runnerUID)
	}
}
