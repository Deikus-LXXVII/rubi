package rubiplugin

import (
	"context"
	"encoding/json"
	"io"
	"testing"
)

// LibraryGet calls the host method library.get with the id and version and decodes the answer.
func TestHostLibraryGet(t *testing.T) {
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	var got struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	core := NewConn(ar, aw, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		if method != "library.get" {
			return nil, &Error{Code: -32601, Message: "nope"}
		}
		_ = json.Unmarshal(params, &got)
		return map[string]any{"id": got.ID, "version": 3, "content": "body", "sha256": "abc", "signature": "sig"}, nil
	})
	h := &Host{conn: NewConn(br, bw, nil)}
	go func() { _ = core.Run(context.Background()) }()
	go func() { _ = h.conn.Run(context.Background()) }()
	defer aw.Close()
	defer bw.Close()
	var out struct {
		ID        string `json:"id"`
		Version   int    `json:"version"`
		Content   string `json:"content"`
		Signature string `json:"signature"`
	}
	if err := h.LibraryGet(context.Background(), "design-tokens", 0, &out); err != nil {
		t.Fatal(err)
	}
	if got.ID != "design-tokens" || got.Version != 0 || out.Version != 3 || out.Content != "body" || out.Signature != "sig" {
		t.Fatalf("sent %+v, got %+v", got, out)
	}
}
