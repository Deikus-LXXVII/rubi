package t3vm

// Streamed sessions: where the VM backend has no tool to hand the process over to (Linux, where Rubi Home runs the
// microVM itself and reaches the guest over its control channel), the shim keeps the connection to Home and relays its
// stdio over it. After Home's answer (StartReply.Stream) both sides write chunks, as on the guest's exec stream: a kind
// byte, a 4-byte big-endian length, the bytes. The shim writes its stdin and the stdin's end; Home writes the process's
// stdout, its stderr and, last, its exit status. Closing the connection from the shim's side ends the process.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Chunk kinds on a streamed session (the guest's values, so the stream reads the same on both hops).
const (
	chunkStdin    byte = 1 // shim to Home
	chunkStdinEOF byte = 2 // shim to Home: no more stdin
	chunkStdout   byte = 3 // Home to shim
	chunkStderr   byte = 4 // Home to shim
	chunkExit     byte = 5 // Home to shim, last: 4 bytes, the exit status (big endian, signed; -1 = killed by a signal)
)

// maxChunk is the largest chunk payload either side writes or reads.
const maxChunk = 32 << 10

var errStream = errors.New("t3vm: stream protocol error")

func writeChunk(w io.Writer, kind byte, b []byte) error {
	for {
		n := min(len(b), maxChunk)
		h := make([]byte, 5, 5+n)
		h[0] = kind
		binary.BigEndian.PutUint32(h[1:], uint32(n))
		if _, err := w.Write(append(h, b[:n]...)); err != nil {
			return err
		}
		b = b[n:]
		if len(b) == 0 {
			return nil
		}
	}
}

func readChunk(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > maxChunk {
		return 0, nil, fmt.Errorf("%w: chunk of %d bytes", errStream, n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return h[0], b, nil
}

// exitStatus is what the shim exits with for the process's status: the status itself, or 137 (128 + SIGKILL, what a
// shell reports) for a process ended by a signal or a status out of range.
func exitStatus(code int32) int {
	if code < 0 || code > 255 {
		return 137
	}
	return int(code)
}

// streamClient relays stdin to Home and Home's stdout and stderr chunks to stdout and stderr until the exit status
// arrives, and returns the status to exit with. A connection that ends without one is a failure (1).
func streamClient(conn io.ReadWriter, stdin io.Reader, stdout, stderr io.Writer) int {
	go func() {
		buf := make([]byte, maxChunk)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				if writeChunk(conn, chunkStdin, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				_ = writeChunk(conn, chunkStdinEOF, nil)
				return
			}
		}
	}()
	for {
		kind, b, err := readChunk(conn)
		if err != nil {
			fmt.Fprintf(stderr, "rubi-home agent-vm: the session with Rubi Home ended: %v\n", err)
			return 1
		}
		switch kind {
		case chunkStdout:
			if _, err := stdout.Write(b); err != nil {
				return 1 // T3 stopped reading: nothing more can be delivered
			}
		case chunkStderr:
			_, _ = stderr.Write(b)
		case chunkExit:
			if len(b) != 4 {
				fmt.Fprintln(stderr, "rubi-home agent-vm: Rubi Home's exit status wasn't understood")
				return 1
			}
			return exitStatus(int32(binary.BigEndian.Uint32(b)))
		default:
			fmt.Fprintln(stderr, "rubi-home agent-vm: Rubi Home sent something unexpected")
			return 1
		}
	}
}

// StreamProc is a process in a VM as Home streams it: reads are its stdout, writes its stdin (guest.Process).
type StreamProc interface {
	io.Reader
	io.Writer
	CloseWrite() error
	Close() error
	Wait() (int, error)
}

// streamOut writes the process's output chunks to the shim; stdout and stderr come from different goroutines.
type streamOut struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *streamOut) write(kind byte, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeChunk(s.w, kind, b)
}

// stderrWriter is the process's stderr, as the guest client wants it (given before the process starts).
func (s *streamOut) stderrWriter() io.Writer { return stderrChunks{s} }

type stderrChunks struct{ s *streamOut }

func (e stderrChunks) Write(b []byte) (int, error) {
	if err := e.s.write(chunkStderr, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// streamServer relays the shim's stdin to p and p's stdout to the shim, then sends p's exit status. It returns when p
// has ended and its status is sent, or when the shim is gone or ctx ends (then p is ended). The caller closes conn.
func streamServer(ctx context.Context, conn io.Reader, out *streamOut, p StreamProc) error {
	shimGone := make(chan struct{})
	go func() {
		defer close(shimGone)
		for {
			kind, b, err := readChunk(conn)
			if err != nil {
				return
			}
			switch kind {
			case chunkStdin:
				if _, err := p.Write(b); err != nil {
					return
				}
			case chunkStdinEOF:
				_ = p.CloseWrite()
			default:
				return
			}
		}
	}()
	copied := make(chan error, 1)
	go func() {
		buf := make([]byte, maxChunk)
		for {
			n, err := p.Read(buf)
			if n > 0 {
				if werr := out.write(chunkStdout, buf[:n]); werr != nil {
					copied <- werr
					return
				}
			}
			if err != nil {
				copied <- nil
				return
			}
		}
	}()
	select {
	case err := <-copied:
		if err != nil { // the shim stopped reading
			_ = p.Close()
			return err
		}
	case <-shimGone:
		// The shim is gone (T3 ended it) or broke the protocol: end the process. Its stdout reader then ends too.
		_ = p.Close()
		<-copied
		return errors.New("t3vm: the shim left before the process ended")
	case <-ctx.Done():
		// Home ends the session (its pipe keeps failing, or Home stops): the process goes with it.
		_ = p.Close()
		<-copied
		return errors.New("t3vm: the session was ended")
	}
	code, err := p.Wait()
	if err != nil {
		code = -1
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(int32(code)))
	return out.write(chunkExit, b[:])
}
