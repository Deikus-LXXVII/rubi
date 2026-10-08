package rubiplugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// MaxMessage is the largest JSON-RPC message either side accepts.
const MaxMessage = 4 << 20

// Handler answers a request from the other side. A nil result is sent as {}.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// Error is a JSON-RPC error. Handlers may return one to choose the code; other errors become code -32000.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Error codes Rubi uses for Rubi Home calls (Host.HomeCall), so plugins can react without matching text.
const (
	CodeHomeOffline   = -32010 // Rubi Home didn't answer (the computer is off, asleep or offline)
	CodeHomeNotPaired = -32011 // no Rubi Home is paired, or it no longer knows this Rubi
	CodeHomeFailed    = -32012 // Rubi Home refused or the operation failed; the message says why
)

// ErrorCode returns the JSON-RPC error code of err, or 0 if it has none.
func ErrorCode(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

// ErrClosed is returned by calls on a connection whose other side went away.
var ErrClosed = errors.New("connection closed")

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Conn is a bidirectional JSON-RPC 2.0 connection with one message per line. Both the host and plugins
// use it: either side can send requests while it is answering one.
type Conn struct {
	r       io.Reader
	handler Handler

	wmu sync.Mutex
	w   *bufio.Writer

	mu      sync.Mutex
	next    int64
	pending map[string]chan message
	closed  bool
	done    chan struct{}
}

func NewConn(r io.Reader, w io.Writer, h Handler) *Conn {
	return &Conn{r: r, w: bufio.NewWriter(w), handler: h, pending: map[string]chan message{}, done: make(chan struct{})}
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Run reads messages until the reader ends. Incoming requests are handled concurrently.
func (c *Conn) Run(ctx context.Context) error {
	defer c.shutdown()
	sc := bufio.NewScanner(c.r)
	sc.Buffer(make([]byte, 64<<10), MaxMessage)
	for sc.Scan() {
		var m message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		switch {
		case m.Method != "":
			go c.serve(ctx, m)
		case len(m.ID) > 0:
			c.mu.Lock()
			ch := c.pending[string(m.ID)]
			delete(c.pending, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}

func (c *Conn) shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	close(c.done)
}

func (c *Conn) serve(ctx context.Context, m message) {
	var res any
	var err error
	if c.handler == nil {
		err = &Error{Code: -32601, Message: "method not found: " + m.Method}
	} else {
		res, err = c.handler(ctx, m.Method, m.Params)
	}
	if len(m.ID) == 0 {
		return // a notification: nobody waits for an answer
	}
	out := message{JSONRPC: "2.0", ID: m.ID}
	if err != nil {
		var e *Error
		if !errors.As(err, &e) {
			e = &Error{Code: -32000, Message: err.Error()}
		}
		out.Error = e
	} else {
		if res == nil {
			res = struct{}{}
		}
		raw, merr := json.Marshal(res)
		if merr != nil {
			out.Error = &Error{Code: -32603, Message: "result is not JSON: " + merr.Error()}
		} else {
			out.Result = raw
		}
	}
	_ = c.write(out)
}

func (c *Conn) write(m message) error {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > MaxMessage {
		return fmt.Errorf("message too large (%d bytes)", len(b))
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return err
	}
	return c.w.Flush()
}

// Call sends a request and decodes the result into result (which may be nil).
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	raw, err := json.Marshal(orEmpty(params))
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.next++
	id := strconv.FormatInt(c.next, 10)
	ch := make(chan message, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(message{ID: json.RawMessage(id), Method: method, Params: raw}); err != nil {
		c.forget(id)
		return err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		if m.Error != nil {
			return m.Error
		}
		if result != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.forget(id)
		return ctx.Err()
	}
}

// Notify sends a message that gets no answer.
func (c *Conn) Notify(method string, params any) error {
	raw, err := json.Marshal(orEmpty(params))
	if err != nil {
		return err
	}
	return c.write(message{Method: method, Params: raw})
}

func (c *Conn) forget(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func orEmpty(v any) any {
	if v == nil {
		return struct{}{}
	}
	return v
}
