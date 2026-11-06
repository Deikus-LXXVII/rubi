package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/sshtarget"
)

// Close ends a question still waiting for the user at once (denied: Home is shutting down), and returns only when the
// request that asked it has finished, so nothing writes into Home's folder after Close (here: the refusal in the SSH
// audit). Before, the question waited out its full time and Close returned while the request was still running.
func TestCloseEndsPendingAsksAndWaitsForTheirRequests(t *testing.T) {
	se := newSSHEnv(t, sshtarget.Limits{Asks: 2})
	se.register("web-1", nil, true)
	_, fr := se.role("deployer", "web-1")
	done := make(chan error, 1)
	go func() { _, err := se.open(fr, "web-1", "uptime"); done <- err }()
	se.pl.wait(t, "ssh.ask")

	start := time.Now()
	se.m.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %v with a question waiting", d)
	}
	raw, _ := os.ReadFile(filepath.Join(se.dir, "agent-ssh", "audit.jsonl"))
	if !strings.Contains(string(raw), `"denied"`) || !strings.Contains(string(raw), "shutting down") {
		t.Fatalf("when Close returned, the request had not written its refusal:\n%s", raw)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the SSH command ran")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the runner's request did not end")
	}
	// after Close, a new request is refused at once
	if _, err := se.op("ssh.list", nil); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("a request after Close: %v", err)
	}
}
