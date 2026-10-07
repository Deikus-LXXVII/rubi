package rubiplugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

// TestConnBothWays: each side can call the other while answering a call (the host asks a plugin to run a
// tool; the plugin asks the host for a secret before answering).
func TestConnBothWays(t *testing.T) {
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	var host *Conn
	host = NewConn(ar, aw, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		if method == "secret.get" {
			return map[string]string{"value": "s3cret"}, nil
		}
		return nil, &Error{Code: -32601, Message: "nope"}
	})
	var plugin *Conn
	plugin = NewConn(br, bw, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		var out struct{ Value string }
		if err := plugin.Call(ctx, "secret.get", nil, &out); err != nil {
			return nil, err
		}
		return map[string]string{"got": out.Value}, nil
	})
	go func() { _ = host.Run(context.Background()) }()
	go func() { _ = plugin.Run(context.Background()) }()

	var res struct{ Got string }
	if err := host.Call(context.Background(), "tool", nil, &res); err != nil || res.Got != "s3cret" {
		t.Fatalf("nested call: %+v %v", res, err)
	}
	err := plugin.Call(context.Background(), "unknown", nil, nil)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("error passthrough: %v", err)
	}
	aw.Close()
	bw.Close()
	<-host.Done()
	if err := host.Call(context.Background(), "tool", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("call after close: %v", err)
	}
}
