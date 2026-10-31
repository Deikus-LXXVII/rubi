package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Fake is a VM whose runner is a FakeRunner in the same process, speaking the runner protocol over
// pipes. It is what the tests of this package and of Rubi Home use instead of a real container.
type Fake struct {
	// Setup configures each FakeRunner before it starts (nil: defaults).
	Setup func(*FakeRunner)
	// StartErr, when set, makes Start fail.
	StartErr error
	// Gateway is the address handed to StartSpec.Prepare ("" by default, as a VM with none to report).
	Gateway string

	mu      sync.Mutex
	state   State
	last    *FakeRunner
	starts  int
	lastEnv map[string]string
}

// State implements VM.
func (f *Fake) State() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == "" {
		return StateStopped
	}
	return f.state
}

// Starts is how many times the runner was started.
func (f *Fake) Starts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

// Runner is the runner of the last start (nil before the first).
func (f *Fake) Runner() *FakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// Env is the environment the last start passed.
func (f *Fake) Env() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastEnv
}

// Start implements VM.
func (f *Fake) Start(ctx context.Context, spec StartSpec) (Conn, error) {
	f.mu.Lock()
	if f.StartErr != nil {
		f.state = StateFailed
		err := f.StartErr
		f.mu.Unlock()
		return nil, err
	}
	f.starts++
	f.mu.Unlock()
	env, err := prepareEnv(ctx, spec, f.Gateway)
	f.mu.Lock()
	if err != nil {
		f.state = StateFailed
		f.mu.Unlock()
		return nil, err
	}
	f.lastEnv = env
	f.state = StateRunning
	f.mu.Unlock()
	spec.Env = env

	hostToRunnerR, hostToRunnerW := io.Pipe()
	runnerToHostR, runnerToHostW := io.Pipe()
	fr := &FakeRunner{Token: spec.Env["RUBI_RUN_TOKEN"], Project: spec.Project, in: hostToRunnerR, out: runnerToHostW,
		exited: make(chan struct{})}
	if f.Setup != nil {
		f.Setup(fr)
	}
	f.mu.Lock()
	f.last = fr
	f.mu.Unlock()
	c := &fakeConn{r: runnerToHostR, w: hostToRunnerW, done: make(chan struct{}), fr: fr}
	go func() {
		fr.run()
		runnerToHostW.Close()
		close(c.done)
	}()
	return c, nil
}

// Exec implements VM; the fake has no other processes.
func (f *Fake) Exec(ctx context.Context, argv []string, env map[string]string) (Conn, error) {
	return nil, errors.New("fake VM: exec isn't supported")
}

// Stop implements VM.
func (f *Fake) Stop(ctx context.Context) error {
	f.mu.Lock()
	f.state = StateStopped
	fr := f.last
	f.mu.Unlock()
	if fr != nil {
		fr.Exit()
	}
	return nil
}

type fakeConn struct {
	r    io.Reader
	w    *io.PipeWriter
	done chan struct{}
	fr   *FakeRunner
}

func (c *fakeConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *fakeConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *fakeConn) CloseWrite() error           { return c.w.Close() }
func (c *fakeConn) Done() <-chan struct{}       { return c.done }
func (c *fakeConn) Close() error                { c.fr.Exit(); return nil }

// Call is a request the fake runner got.
type Call struct {
	Method string
	Params json.RawMessage
}

// FakeRunner plays a runner: hello with the token it was started with, then the answers a real one
// would give, plus helpers for a test to make it speak (Emit, Ask, Credential, Replay).
type FakeRunner struct {
	// Token is the run token from the environment; set BadToken to send a wrong one, NoHello to stay silent.
	Token    string
	BadToken bool
	NoHello  bool
	// ReplaySession is the session id "<uuid-1>" stands for in a replayed transcript.
	ReplaySession string
	Project       Project
	// Handle, when set, answers requests before the defaults (return handled=false to fall through).
	Handle func(method string, params json.RawMessage) (res any, err error, handled bool)

	in     *io.PipeReader
	out    *io.PipeWriter
	peer   *peer
	exited chan struct{}
	once   sync.Once

	mu       sync.Mutex
	calls    []Call
	notes    []Call // notifications Home sent (ssh.data, ssh.exit, ...)
	sessions int
	helloOK  bool
	replies  []json.RawMessage
}

func (fr *FakeRunner) run() {
	defer fr.once.Do(func() { close(fr.exited) })
	fr.peer = newPeer(fr.out)
	fr.peer.onReq = func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		return fr.answer(method, params)
	}
	fr.peer.onNote = func(method string, params json.RawMessage) {
		fr.mu.Lock()
		fr.notes = append(fr.notes, Call{method, append(json.RawMessage(nil), params...)})
		fr.mu.Unlock()
	}
	go fr.peer.run(bufio.NewReader(fr.in))
	if fr.NoHello {
		<-fr.peer.done
		return
	}
	tok := fr.Token
	if fr.BadToken {
		tok = "not-the-token"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var res struct {
		OK bool `json:"ok"`
	}
	if err := fr.peer.call(ctx, "hello", map[string]string{"run_token": tok, "version": "fake"}, &res); err != nil || !res.OK {
		return // a runner whose hello fails exits
	}
	fr.mu.Lock()
	fr.helloOK = true
	fr.mu.Unlock()
	<-fr.peer.done
}

// Exit ends the fake runner as if the process died.
func (fr *FakeRunner) Exit() {
	fr.in.Close()
	fr.out.Close()
	fr.once.Do(func() { close(fr.exited) })
}

// HelloOK reports whether the handshake succeeded.
func (fr *FakeRunner) HelloOK() bool {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return fr.helloOK
}

// Exited is closed when the fake runner has ended.
func (fr *FakeRunner) Exited() <-chan struct{} { return fr.exited }

func (fr *FakeRunner) answer(method string, params json.RawMessage) (any, error) {
	fr.mu.Lock()
	fr.calls = append(fr.calls, Call{method, append(json.RawMessage(nil), params...)})
	fr.mu.Unlock()
	if fr.Handle != nil {
		if res, err, ok := fr.Handle(method, params); ok {
			return res, err
		}
	}
	switch method {
	case "session.list":
		return map[string]any{"sessions": []any{}}, nil
	case "session.messages":
		return map[string]any{"messages": []any{}}, nil
	case "session.start":
		fr.mu.Lock()
		fr.sessions++
		id := fmt.Sprintf("sess-%d", fr.sessions)
		fr.mu.Unlock()
		fr.Emit(map[string]any{"session_id": id, "kind": "status", "status": "busy"})
		return map[string]any{"id": id}, nil
	case "session.send":
		var p struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(params, &p)
		fr.Emit(map[string]any{"session_id": p.ID, "kind": "status", "status": "busy"})
		return map[string]any{"queued": true}, nil
	case "session.fork":
		fr.mu.Lock()
		fr.sessions++
		id := fmt.Sprintf("sess-%d", fr.sessions)
		fr.mu.Unlock()
		return map[string]any{"id": id}, nil
	case "usage.probe":
		return map[string]any{"logged_in": true, "plan": "fake"}, nil
	case "session.rename", "session.interrupt", "session.mode", "session.close":
		return map[string]any{"ok": true}, nil
	}
	return nil, &RPCError{Code: -32601, Message: "method not found"}
}

// Calls returns the requests received so far.
func (fr *FakeRunner) Calls() []Call {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return append([]Call(nil), fr.calls...)
}

// Notes returns the notifications Home sent so far.
func (fr *FakeRunner) Notes() []Call {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return append([]Call(nil), fr.notes...)
}

// Notify sends a notification to Home (ssh.data, ssh.ack, ssh.close).
func (fr *FakeRunner) Notify(method string, params any) {
	b, _ := json.Marshal(params)
	_ = fr.peer.send(rpcMsg{Method: method, Params: b})
}

// Request makes a request of Home (ssh.open) and decodes the result.
func (fr *FakeRunner) Request(ctx context.Context, method string, params, result any) error {
	return fr.peer.call(ctx, method, params, result)
}

// CallsOf returns the requests of one method.
func (fr *FakeRunner) CallsOf(method string) []Call {
	var out []Call
	for _, c := range fr.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// Emit sends an event notification (fields must include kind).
func (fr *FakeRunner) Emit(fields map[string]any) {
	b, _ := json.Marshal(fields)
	_ = fr.peer.send(rpcMsg{Method: "event", Params: b})
}

// Ask sends a perm.ask and waits for Home's answer.
func (fr *FakeRunner) Ask(ctx context.Context, p AskParams) (AskResult, error) {
	var r AskResult
	err := fr.peer.call(ctx, "perm.ask", p, &r)
	return r, err
}

// Cancel sends perm.cancel for an ask the runner gave up on.
func (fr *FakeRunner) Cancel(askID, sessionID string) {
	b, _ := json.Marshal(map[string]string{"ask_id": askID, "session_id": sessionID})
	_ = fr.peer.send(rpcMsg{Method: "perm.cancel", Params: b})
}

// Credential sends a git.credential request.
func (fr *FakeRunner) Credential(ctx context.Context, p CredParams) (CredResult, error) {
	var r CredResult
	err := fr.peer.call(ctx, "git.credential", p, &r)
	return r, err
}

// Replay plays a recorded runner transcript. Two formats are read: plain newline-delimited JSON-RPC as the
// runner wrote it, and the runner's wire transcripts (runner/testdata: {"from":"runner"|"home","msg":...}),
// of which only the runner's own notifications and requests are played (hello, responses and Home's lines
// are skipped). Notifications are sent, requests are made and their results kept (Replies). In the text,
// "$RUN_TOKEN" becomes the token, "<uuid-1>" the session id ReplaySession (sess-1 when empty) and "<uuid-N>"
// "ask-N".
func (fr *FakeRunner) Replay(ctx context.Context, ndjson []byte) error {
	sess := fr.ReplaySession
	if sess == "" {
		sess = "sess-1"
	}
	for _, line := range bytes.Split(ndjson, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		line = bytes.ReplaceAll(line, []byte("$RUN_TOKEN"), []byte(fr.Token))
		line = bytes.ReplaceAll(line, []byte("<uuid-1>"), []byte(sess))
		line = placeholderRE.ReplaceAll(line, []byte("ask-$1"))
		var wire struct {
			From string  `json:"from"`
			Msg  *rpcMsg `json:"msg"`
		}
		var m rpcMsg
		if json.Unmarshal(line, &wire) == nil && wire.From != "" {
			if wire.From != "runner" || wire.Msg == nil || wire.Msg.Method == "" || wire.Msg.Method == "hello" {
				continue
			}
			m = *wire.Msg
		} else if err := json.Unmarshal(line, &m); err != nil || m.Method == "" {
			return fmt.Errorf("replay: bad line %q", line)
		}
		if len(m.ID) == 0 {
			if err := fr.peer.send(rpcMsg{Method: m.Method, Params: m.Params}); err != nil {
				return err
			}
			continue
		}
		var res json.RawMessage
		if err := fr.peer.call(ctx, m.Method, m.Params, &res); err != nil {
			var re *RPCError
			if errors.As(err, &re) {
				res = nil
			} else {
				return err
			}
		}
		fr.mu.Lock()
		fr.replies = append(fr.replies, res)
		fr.mu.Unlock()
	}
	return nil
}

var placeholderRE = regexp.MustCompile(`<uuid-(\d+)>`)

// Replies are the results Home gave to the requests of a replay.
func (fr *FakeRunner) Replies() []json.RawMessage {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return append([]json.RawMessage(nil), fr.replies...)
}

// ReadFixture reads a transcript file.
func ReadFixture(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return []byte(strings.ReplaceAll(string(b), "\r\n", "\n")), nil
}
