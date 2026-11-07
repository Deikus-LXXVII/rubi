package sandbox

import (
	"net/http"
	"strings"
	"testing"
)

func TestHookAcceptsTheTestEvent(t *testing.T) {
	srv, url, err := ListenHook("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	resp, err := http.Post(url, "application/json", strings.NewReader(`{"type":"webhook.test"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	got.Body.Close()
	if got.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status %d", got.StatusCode)
	}
}
