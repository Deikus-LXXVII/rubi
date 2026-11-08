package microvm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// On Linux the sweep finds a VM's process by its pid file and its command line, and kills it; a process whose command
// line doesn't name the folder is left alone.
func TestSweepKillsALeftProcess(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "web")
	_ = os.Mkdir(dir, 0o700)
	// "; true" keeps the shell from becoming sleep (a shell may exec a lone last command), which would drop the folder
	// from the command line the sweep reads
	left := exec.Command("/bin/sh", "-c", "sleep 30; true", filepath.Join(dir, "fs0.sock"))
	other := exec.Command("/bin/sh", "-c", "sleep 30; true", "/elsewhere/fs1.sock")
	for _, c := range []*exec.Cmd{left, other} {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
	}
	defer other.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- left.Wait() }()
	_ = os.WriteFile(filepath.Join(dir, "fs0.pid"), []byte(strconv.Itoa(left.Process.Pid)), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "fs1.pid"), []byte(strconv.Itoa(other.Process.Pid)), 0o600)
	if err := (Sweeper{Root: root}).Sweep(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		left.Process.Kill()
		t.Fatal("the left process still runs")
	}
	if !processOfFolder(other.Process.Pid, "/elsewhere") {
		t.Error("the other process was killed or isn't seen")
	}
}
