//go:build unix

package t3

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestContractBesideTheUsersT3 runs the user's own T3 (on ~/.t3 of a throwaway home, as the desktop app or the
// background service would) and Rubi's server next to it: ours starts on its own home, and a second server on the
// user's home is refused by T3's state-dir lock with exit code 78.
func TestContractBesideTheUsersT3(t *testing.T) {
	if os.Getenv("RUBI_T3_CONTRACT") != "1" || os.Getenv("RUBI_T3_BIN") == "" || os.Getenv("RUBI_T3_NODE") == "" {
		t.Skip("set RUBI_T3_CONTRACT=1 with RUBI_T3_BIN and RUBI_T3_NODE")
	}
	bin, node := os.Getenv("RUBI_T3_BIN"), os.Getenv("RUBI_T3_NODE")
	home := t.TempDir()
	t.Setenv("HOME", home)
	user := filepath.Join(home, ".t3")
	serve := func(port int) *exec.Cmd {
		cmd := exec.Command(node, bin, "serve", "--base-dir", user, "--port", strconv.Itoa(port), "--host", "127.0.0.1", "--no-browser")
		cmd.Env = (&Server{Node: node}).Env()
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return cmd
	}
	users := serve(contractPort(t))
	if err := users.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-users.Process.Pid, syscall.SIGKILL); _ = users.Wait() })
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(user, "userdata", "server-runtime.json")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the user's T3 did not start")
		}
		time.Sleep(200 * time.Millisecond)
	}

	ours := &Server{Node: node, Bin: bin, BaseDir: filepath.Join(home, "rubi-t3"), Port: contractPort(t)}
	t.Cleanup(ours.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := ours.Start(ctx); err != nil {
		t.Fatalf("Rubi's server beside the user's T3: %v", err)
	}

	second := serve(contractPort(t))
	err := second.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 78 {
		t.Errorf("a second server on the user's home: %v, want exit code 78 (T3's state-dir lock)", err)
	}
	if !ours.Alive() {
		t.Error("Rubi's server stopped")
	}
}

func contractPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
