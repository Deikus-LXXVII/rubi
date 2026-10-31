package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// Newline-delimited JSON-RPC 2.0, one object per line, in both directions.

const maxLine = 4 << 20

// maxServing bounds the requests of the other end served at once; more get an error at once.
const maxServing = 64

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return e.Message }

// readLine reads one line of at most maxLine bytes.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > maxLine {
			return nil, errors.New("line too long")
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return line, nil
			}
			return nil, err
		}
		return line, nil
	}
}

// peer is one end of the channel. Requests the other end makes are served by onReq, each in its own
// goroutine (a perm.ask waits for a person); notifications go to onNote in order.
type peer struct {
	w      io.Writer
	wmu    sync.Mutex
	nextID atomic.Int64

	mu      sync.Mutex
	pending map[string]chan rpcMsg
	closed  bool

	onReq  func(ctx context.Context, method string, params json.RawMessage) (any, error)
	onNote func(method string, params json.RawMessage)
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	sem    chan struct{} // bounds the requests served at once
}

func newPeer(w io.Writer) *peer {
	p := &peer{w: w, pending: map[string]chan rpcMsg{}, done: make(chan struct{}), sem: make(chan struct{}, maxServing)}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	return p
}

func (p *peer) send(m rpcMsg) error {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.w.Write(append(b, '\n'))
	return err
}

// run reads until the stream ends; it fails every call still waiting.
func (p *peer) run(r *bufio.Reader) {
	defer func() {
		p.mu.Lock()
		p.closed = true
		for id, ch := range p.pending {
			delete(p.pending, id)
			close(ch)
		}
		p.mu.Unlock()
		p.cancel()
		close(p.done)
	}()
	for {
		line, err := readLine(r)
		if err != nil {
			return
		}
		var m rpcMsg
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		p.dispatch(m)
	}
}

func (p *peer) dispatch(m rpcMsg) {
	switch {
	case m.Method != "" && len(m.ID) > 0:
		select {
		case p.sem <- struct{}{}:
			go func() {
				defer func() { <-p.sem }()
				p.serve(m)
			}()
		default:
			_ = p.send(rpcMsg{ID: m.ID, Error: &RPCError{Code: -32000, Message: "too many requests at once"}})
		}
	case m.Method != "":
		if p.onNote != nil {
			p.onNote(m.Method, m.Params)
		}
	case len(m.ID) > 0:
		p.mu.Lock()
		ch := p.pending[string(m.ID)]
		delete(p.pending, string(m.ID))
		p.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

func (p *peer) serve(m rpcMsg) {
	reply := rpcMsg{ID: m.ID}
	if p.onReq == nil {
		reply.Error = &RPCError{Code: -32601, Message: "method not found"}
	} else if res, err := p.onReq(p.ctx, m.Method, m.Params); err != nil {
		var re *RPCError
		if errors.As(err, &re) {
			reply.Error = re
		} else {
			reply.Error = &RPCError{Code: -32000, Message: err.Error()}
		}
	} else if b, err := json.Marshal(res); err != nil {
		reply.Error = &RPCError{Code: -32603, Message: "internal error"}
	} else {
		reply.Result = b
	}
	_ = p.send(reply)
}

// call sends a request and waits for its answer.
func (p *peer) call(ctx context.Context, method string, params, result any) error {
	id := strconv.FormatInt(p.nextID.Add(1), 10)
	ch := make(chan rpcMsg, 1)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("the runner is gone")
	}
	p.pending[id] = ch
	p.mu.Unlock()
	drop := func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}
	raw, err := json.Marshal(params)
	if err != nil {
		drop()
		return err
	}
	if err := p.send(rpcMsg{ID: json.RawMessage(id), Method: method, Params: raw}); err != nil {
		drop()
		return err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return errors.New("the runner is gone")
		}
		if m.Error != nil {
			return m.Error
		}
		if result != nil && len(m.Result) > 0 {
			if err := json.Unmarshal(m.Result, result); err != nil {
				return fmt.Errorf("bad %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		drop()
		return ctx.Err()
	}
}
