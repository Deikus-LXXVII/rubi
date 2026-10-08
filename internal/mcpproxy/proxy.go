// Package mcpproxy implements `rubi mcp`: the command the agent registers as its MCP server.
//
// It connects stdio to the daemon's Unix socket and starts the daemon if it isn't running. That lazy
// start is what keeps Rubi alive on agent machines that have no init system: any MCP use brings it up
// again, in the locked state.
//
// The proxy also survives daemon restarts (for example into an update): it reconnects, replays the MCP
// handshake the agent already did (hiding the duplicate reply), and answers requests that were in flight
// with an error asking the agent to retry, so the agent's session continues without being re-registered.
package mcpproxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
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
	logf, err := paths.OpenLog(layout.DaemonLog())
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

// envelope is the part of a JSON-RPC message the proxy needs to look at.
type envelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type session struct {
	mu       sync.Mutex
	out      *bufio.Writer
	initReq  []byte          // the agent's initialize request, replayed after a reconnect
	initNote []byte          // its notifications/initialized
	pending  map[string]bool // ids of agent requests without a reply yet
	suppress string          // id of a replayed initialize whose reply the agent must not see
}

func (s *session) write(line []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(line)
	_ = s.out.WriteByte('\n')
	_ = s.out.Flush()
}

// fromAgent records what's needed to restore the session later.
func (s *session) fromAgent(line []byte) {
	var e envelope
	if json.Unmarshal(line, &e) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Method {
	case "initialize":
		s.initReq = append([]byte(nil), line...)
	case "notifications/initialized":
		s.initNote = append([]byte(nil), line...)
	}
	if len(e.ID) > 0 && e.Method != "" {
		s.pending[string(e.ID)] = true
	}
}

// fromDaemon returns false for replies the agent must not see.
func (s *session) fromDaemon(line []byte) bool {
	var e envelope
	if json.Unmarshal(line, &e) != nil || len(e.ID) == 0 || (e.Result == nil && e.Error == nil) {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.suppress != "" && string(e.ID) == s.suppress {
		s.suppress = ""
		return false
	}
	delete(s.pending, string(e.ID))
	return true
}

// failPending answers every in-flight request so the agent doesn't wait forever.
func (s *session) failPending() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.pending))
	for id := range s.pending {
		ids = append(ids, id)
	}
	s.pending = map[string]bool{}
	s.mu.Unlock()
	for _, id := range ids {
		s.write([]byte(`{"jsonrpc":"2.0","id":` + id + `,"error":{"code":-32001,"message":"Rubi restarted (for example into an update) while handling this request. Retry it."}}`))
	}
}

// Run pipes stdin/stdout to the daemon, reconnecting if the daemon restarts.
func Run(layout paths.Layout) error {
	conn, err := Dial(layout)
	if err != nil {
		return err
	}
	s := &session{out: bufio.NewWriter(os.Stdout), pending: map[string]bool{}}

	lines := make(chan []byte)
	go func() {
		r := bufio.NewReaderSize(os.Stdin, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				if line[len(line)-1] == '\n' {
					line = line[:len(line)-1]
				}
				lines <- line
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()

	for {
		daemonDone := make(chan struct{})
		go func(c net.Conn) {
			defer close(daemonDone)
			r := bufio.NewReaderSize(c, 1<<20)
			for {
				line, err := r.ReadBytes('\n')
				if len(line) > 0 {
					line = trimNL(line)
					if s.fromDaemon(line) {
						s.write(line)
					}
				}
				if err != nil {
					return
				}
			}
		}(conn)

		restarted := false
		for !restarted {
			select {
			case line, ok := <-lines:
				if !ok { // the agent closed stdin: let the daemon finish replying, then exit
					if uc, ok := conn.(*net.UnixConn); ok {
						_ = uc.CloseWrite()
					}
					<-daemonDone
					conn.Close()
					return nil
				}
				s.fromAgent(line)
				if _, err := conn.Write(append(line, '\n')); err != nil {
					restarted = true
				}
			case <-daemonDone:
				restarted = true
			}
		}

		conn.Close()
		s.failPending()
		conn, err = reconnect(layout)
		if err != nil {
			return err
		}
		if err := s.replay(conn); err != nil {
			conn.Close()
			return err
		}
	}
}

func reconnect(layout paths.Layout) (net.Conn, error) {
	var last error
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		conn, err := Dial(layout)
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, fmt.Errorf("lost the connection to Rubi and couldn't reconnect: %w", last)
}

// replay restores the MCP session on a new connection by re-sending the agent's handshake.
func (s *session) replay(conn io.Writer) error {
	s.mu.Lock()
	req, note := s.initReq, s.initNote
	var e envelope
	if req != nil && json.Unmarshal(req, &e) == nil {
		s.suppress = string(e.ID)
	}
	s.mu.Unlock()
	if req == nil {
		return nil // the agent hadn't initialized yet; nothing to restore
	}
	for _, m := range [][]byte{req, note} {
		if m == nil {
			continue
		}
		if _, err := conn.Write(append(append([]byte(nil), m...), '\n')); err != nil {
			return err
		}
	}
	return nil
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
