package relay_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/relay/relaytest"
)

func TestRoundTripThroughRelays(t *testing.T) {
	good, broken := relaytest.New(), relaytest.New()
	defer good.Close()
	defer broken.Close()
	broken.Drop = true // one relay silently loses everything; the other is enough
	relays := []string{broken.URL(), good.URL(), "ws://127.0.0.1:1"}

	key, _ := relay.NewKey()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan int, 10)
	srv := &relay.Server{Key: key, Relays: relays, Ready: func(n int) { ready <- n },
		Handle: func(_ context.Context, body []byte) (int, []byte) {
			return 200, []byte("echo:" + string(body))
		}}
	go srv.Run(ctx)
	for n := range ready {
		if n >= 2 {
			break
		}
	}

	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	c, err := relay.Dial(dctx, key.Public(), relays)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	big := strings.Repeat("abcdefghij", 20000) // 200 KB: several chunks each way
	for _, msg := range []string{"hello", big} {
		rctx, rcancel := context.WithTimeout(ctx, 10*time.Second)
		status, body, err := c.Do(rctx, []byte(msg))
		rcancel()
		if err != nil || status != 200 || string(body) != "echo:"+msg {
			t.Fatalf("round trip (%d bytes): status=%d err=%v len=%d", len(msg), status, err, len(body))
		}
	}

	// A client that addresses another Rubi gets nothing.
	other, _ := relay.NewKey()
	c2, err := relay.Dial(dctx, other.Public(), relays)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	rctx, rcancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer rcancel()
	if _, _, err := c2.Do(rctx, []byte("x")); err == nil {
		t.Fatal("got an answer from the wrong Rubi")
	}
}

func TestSignVerify(t *testing.T) {
	k, _ := relay.NewKey()
	e := &relay.Event{CreatedAt: time.Now().Unix(), Kind: relay.Kind, Content: "x"}
	if err := k.Sign(e); err != nil {
		t.Fatal(err)
	}
	if err := relay.Verify(e); err != nil {
		t.Fatal(err)
	}
	e.Content = "y"
	if relay.Verify(e) == nil {
		t.Fatal("tampered event verified")
	}
}

func TestAnnouncements(t *testing.T) {
	r := relaytest.New()
	defer r.Close()
	ann, _ := relay.NewKey()
	stranger, _ := relay.NewKey()
	key, _ := relay.NewKey()
	got := make(chan relay.Announcement, 4)
	ready := make(chan int, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &relay.Server{Key: key, Relays: []string{r.URL()}, Ready: func(n int) { ready <- n },
		Handle:   func(context.Context, []byte) (int, []byte) { return 200, nil },
		Announce: &relay.Announcements{Key: ann.Public(), On: func(a relay.Announcement) { got <- a }}}
	go srv.Run(ctx)
	<-ready
	time.Sleep(200 * time.Millisecond)

	if _, err := relay.Publish(ctx, stranger, []string{r.URL()}, relay.Announcement{Core: "v9.9.9"}); err != nil {
		t.Fatal(err)
	}
	if _, err := relay.Publish(ctx, ann, []string{r.URL()}, relay.Announcement{Core: "v1.2.3", Catalog: "abc"}); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-got:
		if a.Core != "v1.2.3" || a.Catalog != "abc" {
			t.Fatalf("announcement: %+v", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no announcement")
	}
	select {
	case a := <-got:
		t.Fatalf("unexpected announcement: %+v", a)
	case <-time.After(500 * time.Millisecond):
	}
}
