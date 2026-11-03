package t3

// The pinned T3 server, run as a child of Rubi Home on loopback with a minimal environment (T3 hands its own
// environment to the provider process, so nothing of the user's may be in it).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is a T3 server Rubi Home starts and stops.
type Server struct {
	Node    string // the node binary
	Bin     string // the pinned t3 bin (a JS file)
	BaseDir string // T3's data folder
	Port    int

	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan struct{}
	version string
}

// Base is the server's address.
func (s *Server) Base() string { return "http://127.0.0.1:" + strconv.Itoa(s.Port) }

// Env is the whole environment of the server and of the pairing command.
func (s *Server) Env() []string {
	return []string{
		"PATH=" + filepath.Dir(s.Node) + ":/usr/bin:/bin",
		"HOME=" + os.Getenv("HOME"),
		"T3CODE_TELEMETRY_ENABLED=false",
		"OTEL_SDK_DISABLED=true",
	}
}

// tail keeps the last bytes written to it.
type tail struct {
	mu sync.Mutex
	b  []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 2048 {
		t.b = t.b[len(t.b)-2048:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

func (s *Server) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, s.Node, append([]string{s.Bin}, args...)...)
	cmd.Env = s.Env()
	cmd.Dir = s.BaseDir
	return cmd
}

const waitDelay = 3 * time.Second

type runtimeInfo struct {
	PID           int    `json:"pid"`
	Port          int    `json:"port"`
	ServerVersion string `json:"serverVersion"`
}

func (s *Server) runtimeFile() string {
	return filepath.Join(s.BaseDir, "userdata", "server-runtime.json")
}

// LogFile is where the server's output goes. It can hold a pairing link or QR code, so it is private and
// never copied into an error.
func (s *Server) LogFile() string { return filepath.Join(s.BaseDir, "server.log") }

// exited reports whether the started process has ended (and been reaped).
func (s *Server) exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Alive reports whether the server process this value started is still running.
func (s *Server) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil && !s.exited()
}

// Done is closed when the started server process ends. It is nil before Start.
func (s *Server) Done() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// Start runs the server and waits until it answers. It returns the server's own version. A server that
// has since died does not count as running.
func (s *Server) Start(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil {
		if !s.exited() {
			return s.version, errors.New("t3: server already started")
		}
		s.stopLocked()
	}
	if s.Node == "" || s.Bin == "" || s.BaseDir == "" || s.Port <= 0 {
		return "", errors.New("t3: server needs node, bin, base dir and port")
	}
	if err := os.MkdirAll(s.BaseDir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(s.BaseDir, 0o700); err != nil {
		return "", err
	}
	// A leftover file from an earlier run would look like this one.
	if err := os.Remove(s.runtimeFile()); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	lf, err := os.OpenFile(s.LogFile(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer lf.Close()
	if err := lf.Chmod(0o600); err != nil {
		return "", err
	}
	// exec.CommandContext would kill on ctx end; the server must outlive the start call.
	cmd := s.command(context.Background(), "serve", "--base-dir", s.BaseDir, "--port", strconv.Itoa(s.Port),
		"--host", "127.0.0.1", "--no-browser")
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.WaitDelay = waitDelay
	own(cmd)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("t3: start: %w", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	s.cmd, s.done = cmd, done

	fail := func(err error) (string, error) {
		s.stopLocked()
		return "", fmt.Errorf("%w (server output is in %s)", err, s.LogFile())
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		select {
		case <-done:
			return fail(errors.New("t3: server exited while starting"))
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(150 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fail(errors.New("t3: server did not come up in time"))
		}
		var ri runtimeInfo
		b, err := os.ReadFile(s.runtimeFile())
		if err != nil || json.Unmarshal(b, &ri) != nil || ri.Port != s.Port || ri.ServerVersion == "" {
			continue
		}
		if !ours(ri.PID, cmd.Process.Pid) {
			return fail(errors.New("t3: runtime file belongs to another process"))
		}
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port)), time.Second)
		if err != nil {
			continue
		}
		c.Close()
		s.version = ri.ServerVersion
		return s.version, nil
	}
}

// Version is the version the running server reported at start.
func (s *Server) Version() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// Stop ends the server and anything it started, and only those. A server that already ended is just
// forgotten: its process group is not signalled, since the id may belong to someone else by now.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

func (s *Server) stopLocked() {
	if s.cmd == nil {
		return
	}
	stop(s.cmd, s.done)
	s.cmd, s.done, s.version = nil, nil, ""
}

// PairingCode mints a one-time pairing code with the given scopes. The code is never logged.
func (s *Server) PairingCode(ctx context.Context, scopes []string, ttl time.Duration, label string) (string, error) {
	args := []string{"auth", "pairing", "create", "--base-dir", s.BaseDir}
	for _, sc := range scopes {
		args = append(args, "--scope", sc)
	}
	args = append(args, "--ttl", ttlArg(ttl), "--label", label, "--json")
	cmd := s.command(ctx, args...)
	var out bytes.Buffer
	lg := &tail{}
	cmd.Stdout, cmd.Stderr = &out, lg
	if err := cmd.Run(); err != nil {
		if m := lg.String(); m != "" {
			return "", fmt.Errorf("t3: pairing create: %w: %s", err, m)
		}
		return "", fmt.Errorf("t3: pairing create: %w", err)
	}
	var r struct {
		Credential string `json:"credential"`
	}
	if json.Unmarshal(out.Bytes(), &r) != nil || r.Credential == "" {
		return "", errors.New("t3: pairing create gave no credential")
	}
	return r.Credential, nil
}

func ttlArg(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	if s := int(d / time.Second); s > 0 {
		return strconv.Itoa(s) + "s"
	}
	return "1s"
}
