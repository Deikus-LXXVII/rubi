//go:build unix

package t3vm

import (
	"bytes"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Ctrl-C reaches the shim and the command both (they share the console). The shim must stay, holding the connection, until the
// command has ended, and then return the command's own status.
func TestRelayProcessSurvivesAnInterruptWhileTheCommandRuns(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	var out, errb bytes.Buffer
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGINT) // only the shim (this process) gets it
	}()
	code, err := relayProcess(sh, []string{"sh", "-c", "sleep 1; echo done; exit 3"}, nil, strings.NewReader(""), &out, &errb)
	if err != nil || code != 3 || out.String() != "done\n" {
		t.Fatalf("code %d err %v out %q", code, err, out.String())
	}
}
