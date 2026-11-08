package t3vm

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProc is a process that echoes its stdin, upper-cased, to stdout, writes "warn" to stderr, and ends with code
// once its stdin ends (or when closed: -1).
type fakeProc struct {
	stdinR *io.PipeReader
	stdinW *io.PipeWriter
	outR   *io.PipeReader
	outW   *io.PipeWriter
	code   int
	done   chan struct{}
	once   sync.Once
	closed bool
	mu     sync.Mutex
}

func newFakeProc(code int, stderr io.Writer) *fakeProc {
	p := &fakeProc{code: code, done: make(chan struct{})}
	p.stdinR, p.stdinW = io.Pipe()
	p.outR, p.outW = io.Pipe()
	go func() {
		_, _ = stderr.Write([]byte("warn"))
		b, _ := io.ReadAll(p.stdinR)
		_, _ = p.outW.Write(bytes.ToUpper(b))
		p.outW.Close()
		p.once.Do(func() { close(p.done) })
	}()
	return p
}

func (p *fakeProc) Read(b []byte) (int, error)  { return p.outR.Read(b) }
func (p *fakeProc) Write(b []byte) (int, error) { return p.stdinW.Write(b) }
func (p *fakeProc) CloseWrite() error           { return p.stdinW.Close() }
func (p *fakeProc) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.stdinW.CloseWithError(errors.New("killed"))
	p.outW.CloseWithError(errors.New("killed"))
	p.once.Do(func() { close(p.done) })
	return nil
}
func (p *fakeProc) Wait() (int, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return -1, nil
	}
	return p.code, nil
}

// pair runs streamServer on one end of a pipe with a fakeProc and returns the other end.
func pair(t *testing.T, code int) (shim net.Conn, proc *fakeProc, served chan error) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	out := &streamOut{w: b}
	proc = newFakeProc(code, out.stderrWriter())
	served = make(chan error, 1)
	go func() { served <- streamServer(b, out, proc) }()
	return a, proc, served
}

func TestStreamRelaysStdioAndTheExitStatus(t *testing.T) {
	for _, tc := range []struct{ code, want int }{{0, 0}, {3, 3}, {-1, 137}, {300, 137}} {
		shim, _, served := pair(t, tc.code)
		var stdout, stderr bytes.Buffer
		got := streamClient(shim, strings.NewReader("hello"), &stdout, &stderr)
		if got != tc.want || stdout.String() != "HELLO" || stderr.String() != "warn" {
			t.Errorf("code %d: exit %d, stdout %q, stderr %q", tc.code, got, stdout.String(), stderr.String())
		}
		if err := <-served; err != nil {
			t.Errorf("server: %v", err)
		}
	}
}

func TestStreamEndsTheProcessWhenTheShimLeaves(t *testing.T) {
	shim, proc, served := pair(t, 0)
	_ = writeChunk(shim, chunkStdin, []byte("partial"))
	shim.Close()
	select {
	case err := <-served:
		if err == nil {
			t.Error("the server reported a clean end")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server kept going after the shim left")
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if !proc.closed {
		t.Error("the process was left running")
	}
}

func TestStreamRefusesOversizeAndUnknownChunks(t *testing.T) {
	var big bytes.Buffer
	h := []byte{chunkStdout, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(h[1:], maxChunk+1)
	big.Write(h)
	if _, _, err := readChunk(&big); !errors.Is(err, errStream) {
		t.Errorf("oversize chunk: %v", err)
	}
	// a chunk kind only Home may send, from the shim, ends the session
	shim, proc, served := pair(t, 0)
	_ = writeChunk(shim, chunkExit, []byte{0, 0, 0, 0})
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("an unexpected chunk from the shim was ignored")
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if !proc.closed {
		t.Error("the process was left running")
	}
	// Home's side: an unknown kind or a short exit status is a failure
	var s bytes.Buffer
	_ = writeChunk(&s, 9, nil)
	if got := streamClient(rw{&s, io.Discard}, strings.NewReader(""), io.Discard, io.Discard); got != 1 {
		t.Errorf("unknown chunk: exit %d", got)
	}
	s.Reset()
	_ = writeChunk(&s, chunkExit, []byte{1})
	if got := streamClient(rw{&s, io.Discard}, strings.NewReader(""), io.Discard, io.Discard); got != 1 {
		t.Errorf("short exit status: exit %d", got)
	}
	s.Reset() // a connection that ends with no status
	if got := streamClient(rw{&s, io.Discard}, strings.NewReader(""), io.Discard, io.Discard); got != 1 {
		t.Errorf("no exit status: exit %d", got)
	}
}

type rw struct {
	io.Reader
	io.Writer
}

func TestWriteChunkSplitsLargeWrites(t *testing.T) {
	var b bytes.Buffer
	data := bytes.Repeat([]byte("x"), 2*maxChunk+5)
	if err := writeChunk(&b, chunkStdout, data); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for b.Len() > 0 {
		kind, c, err := readChunk(&b)
		if err != nil || kind != chunkStdout || len(c) > maxChunk {
			t.Fatalf("%d %d %v", kind, len(c), err)
		}
		got = append(got, c...)
	}
	if !bytes.Equal(got, data) {
		t.Error("the data changed")
	}
}

// Through Run: Home answers Stream, and the shim relays instead of running a tool.
func TestClientStreamsWhenHomeSaysSo(t *testing.T) {
	d, err := os.MkdirTemp("", "c")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	sock := filepath.Join(d, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		if _, err := br.ReadBytes('\n'); err != nil {
			return
		}
		b, _ := json.Marshal(StartReply{OK: true, Stream: true})
		out := &streamOut{w: c}
		p := newFakeProc(7, out.stderrWriter())
		// the reply and the first output chunk in one write: the shim must not lose what came with the answer
		var first bytes.Buffer
		first.Write(append(b, '\n'))
		_ = writeChunk(&first, chunkStdout, []byte(">"))
		c.Write(first.Bytes())
		_ = streamServer(br, out, p)
	}()
	var stdout, stderr bytes.Buffer
	o := ClientOptions{
		ExecTool: "container", Stdin: strings.NewReader("abc"),
		Getwd:   func() (string, error) { return "/w/proj", nil },
		Environ: func() []string { return nil },
		Exec: func(string, []string, []string, *os.File) error {
			t.Error("a streamed session ran a tool")
			return errors.New("no")
		},
	}
	if code := Run(sock, "r_aaaaaa01", []string{"-p"}, &stdout, &stderr, o); code != 7 || stdout.String() != ">ABC" || stderr.String() != "warn" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}
