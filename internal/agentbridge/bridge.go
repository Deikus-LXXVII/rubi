// Package agentbridge carries fixed-target HTTP/TCP streams over inherited pipes.
// Only the relay is inside srt. No network namespace or Unix-socket escape is used.
package agentbridge

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// Command and Environment are trusted supervisor input, never provider arguments.
// The entire envelope, including credentials, travels only through inherited pipes.
type Bootstrap struct {
	Command                             []string
	Environment                         map[string]string
	Port                                int
	PairCommand                         []string
	ParentNamespaces                    []string
	CheckPath, CheckUser, CheckPassword string
}

func (Bootstrap) String() string   { return "agentbridge.Bootstrap{redacted}" }
func (Bootstrap) GoString() string { return "agentbridge.Bootstrap{redacted}" }

type Ready struct {
	Credential string
	Failure    string
}

func (Ready) String() string   { return "agentbridge.Ready{redacted}" }
func (Ready) GoString() string { return "agentbridge.Ready{redacted}" }

type channel struct {
	io.ReadCloser
	io.WriteCloser
}

func (c channel) Close() error { return errors.Join(c.ReadCloser.Close(), c.WriteCloser.Close()) }
func envelope(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil || len(b) > 1<<20 {
		return errors.New("bridge: invalid bootstrap")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(b)))
	if _, err = w.Write(size[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
func receive(r io.Reader, v any) error {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return errors.New("bridge: channel ended")
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > 1<<20 {
		return errors.New("bridge: invalid envelope length")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return errors.New("bridge: incomplete envelope")
	}
	if json.Unmarshal(b, v) != nil {
		return errors.New("bridge: invalid envelope")
	}
	return nil
}
func namespaces() ([]string, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("bridge: secure PID/network namespaces require Linux")
	}
	var ns []string
	for _, name := range []string{"pid", "net", "mnt"} {
		s, err := os.Readlink("/proc/self/ns/" + name)
		if err != nil {
			return nil, errors.New("bridge: namespace verification unavailable")
		}
		ns = append(ns, s)
	}
	return ns, nil
}

// Process owns a single confined backend. No credentials are printable or logged.
type Process struct {
	mux       *netpipe.Mux
	cancel    context.CancelFunc
	done      chan struct{}
	transport *http.Transport
	mu        sync.Mutex
	active    int
	last      time.Time
	stopped   bool
	status    protocolStatus
}

func (*Process) String() string   { return "agentbridge.Process{private}" }
func (*Process) GoString() string { return "agentbridge.Process{private}" }

// Start validates the actual service executable as well as the relay. The relay
// refuses to execute unless PID, network and mount namespaces differ from ours.
func Start(ctx context.Context, l agentsandbox.Launch, relay string, b Bootstrap) (*Process, Ready, error) {
	var ready Ready
	ns, err := namespaces()
	if err != nil {
		return nil, ready, err
	}
	if len(b.Command) == 0 || !filepath.IsAbs(b.Command[0]) || b.Port < 1 || b.Port > 65535 {
		return nil, ready, errors.New("bridge: invalid backend")
	}
	protected := append(append([]string{}, l.Protected...), b.Command[0])
	if len(b.PairCommand) > 0 {
		protected = append(protected, b.PairCommand[0])
	}
	if _, err := agentsandbox.Build(l.Policy, protected); err != nil {
		return nil, ready, err
	}
	b.ParentNamespaces = ns
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, ready, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, ready, err
	}
	controlR, controlW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, ready, err
	}
	l.Control = controlR
	life, cancel := context.WithCancel(ctx)
	p := &Process{cancel: cancel, done: make(chan struct{}), last: time.Now()}
	l.CLI, l.Args = relay, []string{"--bridge-relay"}
	// All backend diagnostics are deliberately discarded. Some upstream versions
	// print generated passwords; stderr must not become a secret-bearing log.
	go func() {
		defer close(p.done)
		defer controlR.Close()
		defer controlW.Close()
		defer inR.Close()
		defer outW.Close()
		agentsandbox.Run(life, l, inR, outW, &p.status)
	}()
	rw := channel{outR, inW}
	stopCancel := context.AfterFunc(life, func() { controlW.Close(); rw.Close() })
	timer := time.AfterFunc(90*time.Second, func() { cancel(); rw.Close() })
	if err = envelope(inW, b); err == nil {
		err = receive(outR, &ready)
	}
	timer.Stop()
	if err != nil || ready.Failure != "" {
		cancel()
		rw.Close()
		<-p.done
		stopCancel()
		if ready.Failure != "" {
			return nil, Ready{}, errors.New(ready.Failure)
		}
		return nil, Ready{}, errors.New("bridge: sandboxed service failed startup (diagnostics suppressed)")
	}
	p.mux = netpipe.New(rw, netpipe.Config{})
	go func() { p.mux.Run(); cancel(); rw.Close() }()
	p.transport = &http.Transport{DialContext: p.DialContext, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 30 * time.Second}
	return p, ready, nil
}
func (p *Process) Done() <-chan struct{} { return p.done }

func (p *Process) Alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		return false
	default:
		return !p.stopped
	}
}
func (p *Process) Close() error {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.cancel()
	if p.mux != nil {
		p.mux.Close()
	}
	if p.transport != nil {
		p.transport.CloseIdleConnections()
	}
	<-p.done
	return nil
}

// StopIdle atomically excludes new requests before stopping; an in-flight SSE
// response/upgrade pins the service until its body closes.
func (p *Process) StopIdle(now time.Time, ttl time.Duration) bool {
	p.mu.Lock()
	if p.active != 0 || p.stopped || now.Sub(p.last) < ttl {
		p.mu.Unlock()
		return false
	}
	p.stopped = true
	p.mu.Unlock()
	p.Close()
	return true
}
func (p *Process) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if !p.Alive() {
		return nil, errors.New("bridge: backend stopped")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.mux.Open(netpipe.SvcT3)
}
func (p *Process) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil, errors.New("bridge: backend stopped")
	}
	p.active++
	p.last = time.Now()
	p.mu.Unlock()
	end := func() { p.mu.Lock(); p.active--; p.last = time.Now(); p.mu.Unlock() }
	resp, err := p.transport.RoundTrip(r)
	if err != nil {
		end()
		return nil, err
	}
	// Preserve io.ReadWriteCloser for HTTP 101/WebSocket upgrades.
	resp.Body = &responseBody{ReadCloser: resp.Body, end: end}
	return resp, nil
}

type responseBody struct {
	io.ReadCloser
	once sync.Once
	end  func()
}

func (b *responseBody) Close() error { err := b.ReadCloser.Close(); b.once.Do(b.end); return err }
func (b *responseBody) Write(p []byte) (int, error) {
	w, ok := b.ReadCloser.(io.Writer)
	if !ok {
		return 0, errors.New("bridge: response is not upgraded")
	}
	return w.Write(p)
}

// Relay is an executable subcommand, not a policy-selection interface. The
// command envelope comes from the trusted supervisor over stdin after srt starts.
func Relay(in io.ReadCloser, out io.WriteCloser) (result error) {
	readyWritten := false
	defer func() {
		if result != nil && !readyWritten {
			envelope(out, Ready{Failure: result.Error()})
		}
	}()
	if err := blockingStdio(in, out); err != nil {
		return err
	}
	in, out = wrapStdio(in, out)
	var b Bootstrap
	if err := receive(in, &b); err != nil {
		return err
	}
	ns, err := namespaces()
	if err != nil {
		return err
	}
	status, procErr := os.ReadFile("/proc/self/status")
	if procErr != nil || !privateProc(string(status), os.Getpid()) {
		return errors.New("bridge: private proc filesystem required")
	}
	if len(b.ParentNamespaces) != len(ns) {
		return errors.New("bridge: no namespace attestation")
	}
	for i, n := range ns {
		if n == b.ParentNamespaces[i] {
			return errors.New("bridge: refusing service outside private namespaces")
		}
	}
	if len(b.Command) == 0 || !filepath.IsAbs(b.Command[0]) || b.Port < 1 || b.Port > 65535 {
		return errors.New("bridge: invalid backend")
	}
	child := exec.Command(b.Command[0], b.Command[1:]...)
	child.Env = os.Environ()
	for k, v := range b.Environment {
		child.Env = append(child.Env, k+"="+v)
	}
	child.Stdout, child.Stderr = io.Discard, io.Discard
	configureChild(child)
	if child.Start() != nil {
		return errors.New("bridge: backend did not start")
	}
	exited := make(chan struct{})
	go func() { child.Wait(); close(exited) }()
	defer func() { killChild(child); <-exited }()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(b.Port))
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		c, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		select {
		case <-exited:
			return errors.New("bridge: backend exited before ready")
		case <-deadline.C:
			return errors.New("bridge: backend readiness timed out")
		case <-tick.C:
		}
	}
	if b.CheckPath != "" {
		health, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
		defer client.CloseIdleConnections()
		lastStatus := 0
		for {
			req, err := http.NewRequestWithContext(health, "GET", "http://"+address+b.CheckPath, nil)
			if err != nil {
				return errors.New("bridge: invalid health request")
			}
			req.SetBasicAuth(b.CheckUser, b.CheckPassword)
			resp, err := client.Do(req)
			if err == nil {
				lastStatus = resp.StatusCode
				io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				if lastStatus == 200 {
					break
				}
				if lastStatus == 401 || lastStatus == 403 || lastStatus == 404 {
					return fmt.Errorf("bridge: backend rejected authenticated health request (HTTP %d)", lastStatus)
				}
			}
			select {
			case <-exited:
				return errors.New("bridge: backend exited before authenticated readiness")
			case <-health.Done():
				return fmt.Errorf("bridge: backend authenticated readiness timed out (last HTTP %d)", lastStatus)
			case <-tick.C:
			}
		}
	}

	var ready Ready
	if len(b.PairCommand) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		pair := exec.CommandContext(ctx, b.PairCommand[0], b.PairCommand[1:]...)
		pair.Env = child.Env
		// Bound stdout before decoding; it may contain the one-time pairing credential.
		var result limitedOutput
		pair.Stdout = &result
		pair.Stderr = io.Discard
		if pair.Run() != nil || json.Unmarshal(result.b, &ready) != nil || ready.Credential == "" {
			return errors.New("bridge: native pairing failed")
		}
	}
	if envelope(out, ready) != nil {
		return errors.New("bridge: owner disappeared")
	}
	readyWritten = true
	rw := channel{in, out}
	mux := netpipe.New(rw, netpipe.Config{OnOpen: func(svc byte, s *netpipe.Stream) bool {
		if svc != netpipe.SvcT3 {
			return false
		}
		go func() {
			defer s.Close()
			c, err := net.DialTimeout("tcp", address, 5*time.Second)
			if err != nil {
				return
			}
			defer c.Close()
			finished := make(chan struct{})
			go func() {
				io.Copy(c, s)
				if tcp, ok := c.(*net.TCPConn); ok {
					tcp.CloseWrite()
				}
				close(finished)
			}()
			io.Copy(s, c)
			s.CloseWrite()
			s.Close()
			c.Close()
			<-finished
		}()
		return true
	}})
	go mux.Run()
	select {
	case <-mux.Done():
	case <-exited:
		mux.Close()
		return errors.New("bridge: backend exited")
	}
	err = mux.Err()
	mux.Close()
	return err
}

type limitedOutput struct{ b []byte }

func (w *limitedOutput) Write(p []byte) (int, error) {
	if len(w.b)+len(p) > 1<<20 {
		return 0, errors.New("bridge: oversized pairing result")
	}
	w.b = append(w.b, p...)
	return len(p), nil
}

// NSpid is relative to the namespace owning the proc mount. Exactly one PID
// proves fresh proc; /proc/1/ns may be unreadable for bwrap's nondumpable init.
func privateProc(status string, pid int) bool {
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "NSpid:") {
			fields := strings.Fields(line)
			return len(fields) == 2 && fields[1] == strconv.Itoa(pid)
		}
	}
	return false
}

func (p *Process) Err() error { return p.mux.Err() }

// Only diagnostics emitted by our relay are retained; backend and srt output
// cannot become a secret-bearing log. Relay diagnostics contain no input data.
type protocolStatus struct {
	mu      sync.Mutex
	pending string
	last    string
}

func (w *protocolStatus) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending += string(p)
	if len(w.pending) > 8192 {
		w.pending = ""
	}
	for {
		line, rest, ok := strings.Cut(w.pending, "\n")
		if !ok {
			break
		}
		w.pending = rest
		if strings.HasPrefix(line, "rubi-relay: ") {
			w.last = strings.TrimPrefix(line, "rubi-relay: ")
		}
	}
	return len(p), nil
}
func (p *Process) Diagnostic() string {
	p.status.mu.Lock()
	defer p.status.mu.Unlock()
	return p.status.last
}
