package guest

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// Client is Home's side of a VM's control channel.
type Client struct {
	mux *netpipe.Mux
}

// NewClient serves the channel (until it ends or Close). Wait for Ready before the first request.
func NewClient(rw io.ReadWriteCloser, logf func(string, ...any)) *Client {
	c := &Client{mux: netpipe.New(rw, netpipe.Config{Logf: logf})} // OnOpen nil: the guest opens nothing
	go func() { _ = c.mux.Run() }()
	return c
}

// Ready waits for the guest agent's HELLO.
func (c *Client) Ready(ctx context.Context) error {
	select {
	case <-c.mux.Hello():
		return nil
	case <-c.mux.Done():
		return fmt.Errorf("guest: the control channel ended before the guest agent was up: %w", c.mux.Err())
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done is closed when the channel has ended (the VM is gone).
func (c *Client) Done() <-chan struct{} { return c.mux.Done() }

// Close ends the channel.
func (c *Client) Close() error { return c.mux.Close() }

// control sends one request on a stream of its own and reads the answer.
func (c *Client) control(ctx context.Context, req ControlRequest) error {
	st, err := c.mux.Open(SvcControl)
	if err != nil {
		return err
	}
	defer st.Close()
	stop := context.AfterFunc(ctx, func() { _ = st.Close() })
	defer stop()
	if err := writeLine(st, req); err != nil {
		return err
	}
	line, err := readLine(st)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("guest: no answer to %s: %w", req.Op, err)
	}
	var rep ControlReply
	if err := json.Unmarshal(line, &rep); err != nil {
		return fmt.Errorf("%w: the answer to %s", ErrProtocol, req.Op)
	}
	if rep.Error != "" {
		return fmt.Errorf("guest: %s: %s", req.Op, clip(rep.Error))
	}
	return nil
}

// Mount has the guest mount the shared folders, in order.
func (c *Client) Mount(ctx context.Context, ms []Mount) error {
	return c.control(ctx, ControlRequest{Op: "mount", Mounts: ms})
}

// PowerOff has the guest end the VM.
func (c *Client) PowerOff(ctx context.Context) error {
	return c.control(ctx, ControlRequest{Op: "poweroff"})
}

// Ping checks the guest agent answers.
func (c *Client) Ping(ctx context.Context) error { return c.control(ctx, ControlRequest{Op: "ping"}) }

// Exec starts a process in the VM. Its stdout is what Process reads, and its stderr goes to stderr (nil discards).
func (c *Client) Exec(ctx context.Context, req ExecRequest, stderr io.Writer) (*Process, error) {
	if req.UID <= 0 || req.GID <= 0 {
		return nil, errors.New("guest: a process doesn't run as root")
	}
	st, err := c.mux.Open(SvcExec)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = st.Close() })
	defer stop()
	fail := func(err error) (*Process, error) {
		_ = st.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if err := writeLine(st, req); err != nil {
		return fail(err)
	}
	line, err := readLine(st)
	if err != nil {
		return fail(fmt.Errorf("guest: no answer to exec: %w", err))
	}
	var started ExecStarted
	if err := json.Unmarshal(line, &started); err != nil {
		return fail(fmt.Errorf("%w: the answer to exec", ErrProtocol))
	}
	if started.Error != "" {
		return fail(fmt.Errorf("guest: %s: %s", req.Argv[0], clip(started.Error)))
	}
	if stderr == nil {
		stderr = io.Discard
	}
	pr, pw := io.Pipe()
	p := &Process{st: st, out: pr, done: make(chan struct{}), code: -1}
	go p.read(pw, stderr)
	return p, nil
}

// Process is a process in the VM: reads are its stdout, writes its stdin. It has the methods of agent.Conn.
type Process struct {
	st   *netpipe.Stream
	out  *io.PipeReader
	wmu  sync.Mutex
	done chan struct{}
	once sync.Once

	mu   sync.Mutex
	code int
	err  error
}

func (p *Process) read(stdout *io.PipeWriter, stderr io.Writer) {
	err := func() error {
		for {
			kind, b, err := readChunk(p.st)
			if err != nil {
				return fmt.Errorf("guest: the process's stream ended without its exit status: %w", err)
			}
			switch kind {
			case ChunkStdout:
				if _, err := stdout.Write(b); err != nil {
					return err // the reader closed: Close
				}
			case ChunkStderr:
				_, _ = stderr.Write(b)
			case ChunkExit:
				if len(b) != 4 {
					return fmt.Errorf("%w: an exit status of %d bytes", ErrProtocol, len(b))
				}
				p.mu.Lock()
				p.code = int(int32(binary.BigEndian.Uint32(b)))
				p.mu.Unlock()
				return nil
			default:
				return fmt.Errorf("%w: chunk kind %d from the guest", ErrProtocol, kind)
			}
		}
	}()
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
	if err != nil {
		_ = p.st.Close()
		stdout.CloseWithError(err)
	} else {
		stdout.Close()
	}
	p.once.Do(func() { close(p.done) })
}

// Read reads the process's stdout.
func (p *Process) Read(b []byte) (int, error) { return p.out.Read(b) }

// Write writes to the process's stdin.
func (p *Process) Write(b []byte) (int, error) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if err := writeChunks(p.st, ChunkStdin, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// CloseWrite ends the process's stdin.
func (p *Process) CloseWrite() error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	return writeChunk(p.st, ChunkStdinEOF, nil)
}

// Close ends the process (the guest kills it if it still runs) and the stream.
func (p *Process) Close() error {
	err := p.st.Close()
	_ = p.out.Close()
	return err
}

// Done is closed when the process has ended (or its stream has).
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait waits for the end and returns the exit status (-1: killed, or no status came) and why there was none.
func (p *Process) Wait() (int, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code, p.err
}

// clip shortens a message from the guest for an error.
func clip(s string) string {
	r := []rune(s)
	if len(r) > 300 {
		return string(r[:300]) + "..."
	}
	return s
}
