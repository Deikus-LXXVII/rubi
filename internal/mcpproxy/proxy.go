// Package mcpproxy implements `rubi mcp`: the command the agent registers as its MCP server.
//
// It connects stdio to the daemon's Unix socket and starts the daemon if it isn't running. That lazy
// start is what keeps Rubi alive on agent machines that have no init system: any MCP use brings it up
// again, in the locked state.
package mcpproxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/paths"
)

// Dial connects to the daemon, starting it first if needed.
func Dial(layout paths.Layout) (net.Conn, error) {
	if conn, err := net.Dial("unix", layout.Socket()); err == nil {
		return conn, nil
	}
	if err := layout.Ensure(); err != nil {
		return nil, err
	}
	if err := startDaemon(layout); err != nil {
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", layout.Socket()); err == nil {
			return conn, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, errors.New("daemon did not come up within 10s; see " + layout.DaemonLog())
}

func startDaemon(layout paths.Layout) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(layout.DaemonLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "daemon")
	cmd.Env = append(os.Environ(), "RUBI_HOME="+layout.Home)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive the agent's MCP process exiting
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// Run pipes stdin/stdout to the daemon until either side closes.
func Run(layout paths.Layout) error {
	conn, err := Dial(layout)
	if err != nil {
		return err
	}
	defer conn.Close()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(conn, os.Stdin)
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
		close(done)
	}()
	_, err = io.Copy(os.Stdout, conn)
	return err
}
