package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Deikus-LXXVII/rubi/internal/relay"
)

func wsURL(s *httptest.Server) string { return "ws" + strings.TrimPrefix(s.URL, "http") }

func TestGatewayCarriesRubiTraffic(t *testing.T) {
	ann, _ := relay.NewKey()
	g := New(ann.Public())
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	url := wsURL(srv)

	// A Rubi and a panel talk through the gateway.
	rubiKey, _ := relay.NewKey()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan int, 10)
	go (&relay.Server{Key: rubiKey, Relays: []string{url}, Ready: func(n int) { ready <- n },
		Handle:   func(_ context.Context, b []byte) (int, []byte) { return 200, append([]byte("ok:"), b...) },
		Announce: &relay.Announcements{Key: ann.Public(), On: func(relay.Announcement) {}}}).Run(ctx)
	<-ready
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	c, err := relay.Dial(dctx, rubiKey.Public(), []string{url})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	big := strings.Repeat("x", 90000)
	status, body, err := c.Do(dctx, []byte(big))
	if err != nil || status != 200 || string(body) != "ok:"+big {
		t.Fatalf("round trip: %d %v len=%d", status, err, len(body))
	}

	// Anything that isn't Rubi traffic is refused.
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	send := func(e *relay.Event) string {
		b, _ := json.Marshal([]any{"EVENT", e})
		_ = ws.Write(ctx, websocket.MessageText, b)
		_, reply, _ := ws.Read(ctx)
		return string(reply)
	}
	stranger, _ := relay.NewKey()
	note := &relay.Event{CreatedAt: time.Now().Unix(), Kind: 1, Content: "spam"}
	_ = stranger.Sign(note)
	if r := send(note); !strings.Contains(r, "false") {
		t.Fatalf("kind 1 accepted: %s", r)
	}
	fake := &relay.Event{CreatedAt: time.Now().Unix(), Kind: relay.AnnounceKind, Tags: [][]string{{"d", relay.AnnounceTag}}, Content: `{"core":"v9"}`}
	_ = stranger.Sign(fake)
	if r := send(fake); !strings.Contains(r, "false") {
		t.Fatalf("announcement from another key accepted: %s", r)
	}
	noRecipient := &relay.Event{CreatedAt: time.Now().Unix(), Kind: relay.Kind, Content: "x"}
	_ = stranger.Sign(noRecipient)
	if r := send(noRecipient); !strings.Contains(r, "false") {
		t.Fatalf("panel event without recipient accepted: %s", r)
	}
	forged := &relay.Event{CreatedAt: time.Now().Unix(), Kind: relay.Kind, Tags: [][]string{{"p", "ab"}}, Content: "x"}
	_ = stranger.Sign(forged)
	forged.Content = "changed"
	if r := send(forged); !strings.Contains(r, "false") {
		t.Fatalf("forged event accepted: %s", r)
	}

	// The real announcement is kept and handed to a later subscriber.
	if _, err := relay.Publish(ctx, ann, []string{url}, relay.Announcement{Core: "v1.2.3"}); err != nil {
		t.Fatal(err)
	}
	ws2, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws2.CloseNow()
	req, _ := json.Marshal([]any{"REQ", "a", map[string]any{"kinds": []int{relay.AnnounceKind}, "authors": []string{ann.Public()}}})
	_ = ws2.Write(ctx, websocket.MessageText, req)
	_, first, _ := ws2.Read(ctx)
	if !strings.Contains(string(first), "v1.2.3") {
		t.Fatalf("stored announcement not served: %s", first)
	}
}

func TestGatewayLimitsConnectionsPerAddress(t *testing.T) {
	g := New("")
	g.Limits.MaxConnsPerIP = 2
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	ctx := context.Background()
	var open []*websocket.Conn
	for i := 0; i < 2; i++ {
		ws, _, err := websocket.Dial(ctx, wsURL(srv), nil)
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, ws)
	}
	if _, _, err := websocket.Dial(ctx, wsURL(srv), nil); err == nil {
		t.Fatal("third connection from one address accepted")
	}
	for _, ws := range open {
		ws.CloseNow()
	}
}
