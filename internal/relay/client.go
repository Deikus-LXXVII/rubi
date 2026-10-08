package relay

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// Client sends panel requests to a Rubi through relays. The web panel has its own implementation in
// JavaScript; this one serves tests and `rubi dev-panel`.
type Client struct {
	key    *Key
	rubi   string
	pool   *pool
	asm    *assembler
	mu     sync.Mutex
	waits  map[string]chan reply
	cancel context.CancelFunc
}

type reply struct {
	status int
	body   []byte
}

// Dial connects to relays and waits until at least one is ready.
func Dial(ctx context.Context, rubiPub string, relays []string) (*Client, error) {
	key, err := NewKey()
	if err != nil {
		return nil, err
	}
	c := &Client{key: key, rubi: rubiPub, asm: newAssembler(), waits: map[string]chan reply{}}
	ready := make(chan struct{}, 1)
	c.pool = newPool(relays, func(string) []map[string]any {
		return []map[string]any{{"kinds": []int{Kind}, "authors": []string{rubiPub}, "#p": []string{key.Public()},
			"since": time.Now().Add(-time.Minute).Unix()}}
	}, c.onEvent, nil)
	c.pool.ready = func(n int) {
		if n > 0 {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
	}
	rctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go c.pool.run(rctx)
	select {
	case <-ready:
		time.Sleep(300 * time.Millisecond) // let the subscription settle on the relay
		return c, nil
	case <-ctx.Done():
		cancel()
		return nil, errors.New("no relay reachable")
	}
}

func (c *Client) Close() { c.cancel() }

func (c *Client) onEvent(e *Event) {
	if e.Kind != Kind || e.PubKey != c.rubi {
		return
	}
	var pt part
	if json.Unmarshal([]byte(e.Content), &pt) != nil {
		return
	}
	payload, status, done := c.asm.add(e.PubKey, pt)
	if !done {
		return
	}
	c.mu.Lock()
	ch := c.waits[pt.R]
	delete(c.waits, pt.R)
	c.mu.Unlock()
	if ch != nil {
		ch <- reply{status: status, body: []byte(payload)}
	}
}

// Do sends one request body and returns the response body.
func (c *Client) Do(ctx context.Context, body []byte) (int, []byte, error) {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	id := base64.RawURLEncoding.EncodeToString(b)
	ch := make(chan reply, 1)
	c.mu.Lock()
	c.waits[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waits, id)
		c.mu.Unlock()
	}()
	parts, err := split(id, 0, string(body))
	if err != nil {
		return 0, nil, err
	}
	events, err := signParts(c.key, c.rubi, parts)
	if err != nil {
		return 0, nil, err
	}
	if err := c.pool.publish(ctx, events); err != nil {
		return 0, nil, err
	}
	select {
	case r := <-ch:
		return r.status, r.body, nil
	case <-ctx.Done():
		return 0, nil, errors.New("no answer from Rubi through the relays")
	}
}
