package t3vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The test binary plays three parts: the test, the shim (it runs relayProcess) and the command the shim runs (it records its
// process id and waits).
func TestRelayHelperParts(t *testing.T) {
	switch os.Getenv("RELAY_PART") {
	case "shim":
		exe, _ := os.Executable()
		_, _ = relayProcess(exe, []string{"cmd", "-test.run=TestRelayHelperParts"}, append(os.Environ(), "RELAY_PART=command"), strings.NewReader(""), os.Stdout, os.Stderr)
	case "command":
		_ = os.WriteFile(os.Getenv("RELAY_PIDFILE"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		time.Sleep(time.Minute)
	}
}

// Killing the shim kills the command it runs: the command is in a job tied to the shim.
func TestRelayCommandDiesWithTheShim(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(t.TempDir(), "pid")
	shim := exec.Command(exe, "-test.run=TestRelayHelperParts")
	shim.Env = append(os.Environ(), "RELAY_PART=shim", "RELAY_PIDFILE="+pidfile)
	if err := shim.Start(); err != nil {
		t.Fatal(err)
	}
	defer shim.Process.Kill()
	var pid uint32
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if b, err := os.ReadFile(pidfile); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 0 {
				pid = uint32(n)
				break
			}
		}
	}
	if pid == 0 {
		t.Fatal("the command never started")
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		t.Fatalf("the command is not running: %v", err)
	}
	defer windows.CloseHandle(h)
	if ev, _ := windows.WaitForSingleObject(h, 0); ev != uint32(windows.WAIT_TIMEOUT) {
		t.Fatal("the command ended on its own")
	}
	if err := shim.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if ev, err := windows.WaitForSingleObject(h, 15000); err != nil || ev != windows.WAIT_OBJECT_0 {
		t.Fatalf("the command outlived the shim (wait %d, %v)", ev, err)
	}
}
