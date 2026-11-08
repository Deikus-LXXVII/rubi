//go:build !linux

package agentsandbox

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestSupervisedRuntimeRefusesBeforeExecOnOtherPlatforms(t *testing.T) {
	cmd := exec.Command("nonexistent-test-fixture")
	if err := runConfined(context.Background(), cmd, true); err == nil || !strings.Contains(err.Error(), "requires Linux") {
		t.Fatal("unowned runtime was not refused before execution")
	}
}
