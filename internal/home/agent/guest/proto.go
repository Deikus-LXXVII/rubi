// Package guest is the guest agent of the Linux microvm backend (docs/design/linux-microvm.md): the program that runs as
// PID 1 in an agent's VM (cmd/rubi-guest) and Home's client for it. They talk over the VM's one control channel, a
// virtio-serial port, multiplexed with package netpipe's frames. Home opens every stream and the guest opens none, so the
// two sides never pick the same stream number.
//
// Two services:
//
//   - SvcControl: one stream of JSON lines, a request from Home and the guest's answer for each (mount the shared folders,
//     power off).
//   - SvcExec: one stream per process. Home writes an ExecRequest as one JSON line, the guest answers one JSON line
//     (ExecStarted), and then both sides write chunks (a kind byte, a 4-byte big-endian length, the bytes): Home the
//     process's stdin and its end, the guest its stdout, its stderr and, last, its exit status. Closing the stream
//     from Home's side kills the process.
//
// The guest is the less trusted side: what runs in it may have taken over the VM's root. Home's client bounds what it
// reads (line and chunk sizes) and gives the guest nothing but what the request carries.
package guest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Services on the control channel.
const (
	SvcControl byte = 1
	SvcExec    byte = 2
)

// Chunk kinds on an exec stream.
const (
	ChunkStdin    byte = 1 // Home to guest
	ChunkStdinEOF byte = 2 // Home to guest: no more stdin
	ChunkStdout   byte = 3 // guest to Home
	ChunkStderr   byte = 4 // guest to Home
	ChunkExit     byte = 5 // guest to Home, last: 4 bytes, the exit status (big endian, signed; -1 = killed by a signal)
)

// MaxChunk is the largest chunk payload; MaxLine the longest JSON line either side reads.
const (
	MaxChunk = 32 << 10
	MaxLine  = 64 << 10
)

// ErrProtocol is a stream that does not follow the protocol.
var ErrProtocol = errors.New("guest: protocol error")

// Mount is one shared folder the guest mounts (microvm.GuestMount).
type Mount struct {
	Tag      string `json:"tag"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

// ControlRequest is a request on the control stream.
type ControlRequest struct {
	Op     string  `json:"op"` // "mount", "poweroff", "ping"
	Mounts []Mount `json:"mounts,omitempty"`
}

// ControlReply answers it; Error is empty when it worked.
type ControlReply struct {
	Error string `json:"error,omitempty"`
}

// ExecRequest starts a process. Env is the whole environment (nothing is inherited); Dir is where it starts; UID and GID
// are who it runs as (never 0: the guest refuses root).
type ExecRequest struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env,omitempty"`
	Dir  string            `json:"dir"`
	UID  int               `json:"uid"`
	GID  int               `json:"gid"`
}

// validID is a user or group id a process may run as: not root, and one that fits the kernel's 32 bits (a larger
// number would wrap, maybe to 0) without being the "no id" value.
func validID(id int) bool { return id >= 1 && int64(id) < 1<<32-1 }

// ExecStarted answers it; Error is empty when the process runs.
type ExecStarted struct {
	Error string `json:"error,omitempty"`
}

// writeChunk writes one chunk.
func writeChunk(w io.Writer, kind byte, b []byte) error {
	if len(b) > MaxChunk {
		return fmt.Errorf("%w: chunk of %d bytes", ErrProtocol, len(b))
	}
	h := make([]byte, 5, 5+len(b))
	h[0] = kind
	binary.BigEndian.PutUint32(h[1:], uint32(len(b)))
	_, err := w.Write(append(h, b...))
	return err
}

// writeChunks writes b as as many chunks of kind as it takes.
func writeChunks(w io.Writer, kind byte, b []byte) error {
	for len(b) > 0 {
		n := min(len(b), MaxChunk)
		if err := writeChunk(w, kind, b[:n]); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// readChunk reads one chunk; a payload over MaxChunk is a protocol error.
func readChunk(r io.Reader) (kind byte, b []byte, err error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxChunk {
		return 0, nil, fmt.Errorf("%w: chunk of %d bytes", ErrProtocol, n)
	}
	b = make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return h[0], b, nil
}

// readLine reads one line of at most MaxLine bytes, without the newline, a byte at a time (nothing after it is consumed:
// the chunks follow on the same stream).
func readLine(r io.Reader) ([]byte, error) {
	var line []byte
	var c [1]byte
	for {
		if _, err := io.ReadFull(r, c[:]); err != nil {
			if err == io.EOF && len(line) > 0 {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if c[0] == '\n' {
			return line, nil
		}
		if len(line) >= MaxLine {
			return nil, fmt.Errorf("%w: a line over %d bytes", ErrProtocol, MaxLine)
		}
		line = append(line, c[0])
	}
}
