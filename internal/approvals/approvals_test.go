package approvals

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func engine(level Level, c *clock) *Engine {
	e := New(Config{TTL: 15 * time.Minute, MinDelay: 3 * time.Second, MaxPerHour: 2}, Hooks{
		Level: func(string) Level { return level },
		Link:  func(id string) (string, error) { return "https://rubi-panel.com/#a=" + id, nil },
	})
	e.now = c.now
	return e
}

func sendRequest(runs *atomic.Int32, chosen *string) Request {
	return Request{
		Integration: "icloud-mail", Kind: "icloud-mail.send", Summary: "Send email to anna@example.com",
		Question: "Notify you when a reply arrives?",
		Options:  []Option{{Key: "send", Label: "Send"}, {Key: "send_track", Label: "Send and notify on reply"}},
		Execute: func(_ context.Context, opt string) (any, error) {
			runs.Add(1)
			*chosen = opt
			return map[string]string{"status": "sent"}, nil
		},
	}
}

func TestNoneExecutesImmediately(t *testing.T) {
	c := &clock{time.Now()}
	var runs atomic.Int32
	var chosen string
	out, err := engine(None, c).Submit(context.Background(), sendRequest(&runs, &chosen))
	if err != nil || out["status"] != "done" || runs.Load() != 1 || chosen != "send" {
		t.Fatalf("out=%v err=%v runs=%d chosen=%s", out, err, runs.Load(), chosen)
	}
}

func TestChatFlow(t *testing.T) {
	c := &clock{time.Now()}
	e := engine(Chat, c)
	var runs atomic.Int32
	var chosen string
	out, err := e.Submit(context.Background(), sendRequest(&runs, &chosen))
	if err != nil || runs.Load() != 0 {
		t.Fatalf("submit: %v runs=%d", err, runs.Load())
	}
	id := out["approval_id"].(string)
	buttons := out["buttons"].([]map[string]string)
	if len(buttons) != 4 {
		t.Fatalf("want 4 buttons, got %v", buttons)
	}
	for _, b := range buttons {
		for _, r := range b["label"] {
			if r > 0x2000 && r != '·' {
				t.Fatalf("non-plain label %q", b["label"])
			}
		}
	}
	trackLabel := buttons[1]["label"]

	if _, err := e.ConfirmChat(context.Background(), id, trackLabel); err == nil || !strings.Contains(err.Error(), "too soon") {
		t.Fatalf("early confirm: %v", err)
	}
	c.add(5 * time.Second)
	if _, err := e.ConfirmChat(context.Background(), id, "yes"); err == nil {
		t.Fatal("typed 'yes' accepted")
	}
	if _, err := e.ConfirmChat(context.Background(), "apr_bogus", trackLabel); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown id: %v", err)
	}
	snap, err := e.ConfirmChat(context.Background(), id, trackLabel)
	if err != nil || snap.State != Executed || chosen != "send_track" || runs.Load() != 1 {
		t.Fatalf("confirm: %+v err=%v chosen=%s runs=%d", snap, err, chosen, runs.Load())
	}
	if _, err := e.ConfirmChat(context.Background(), id, trackLabel); err == nil {
		t.Fatal("replay accepted")
	}
	if runs.Load() != 1 {
		t.Fatal("executed twice")
	}
}

func TestChatCancelButton(t *testing.T) {
	c := &clock{time.Now()}
	e := engine(Chat, c)
	var runs atomic.Int32
	var chosen string
	out, _ := e.Submit(context.Background(), sendRequest(&runs, &chosen))
	buttons := out["buttons"].([]map[string]string)
	snap, err := e.ConfirmChat(context.Background(), out["approval_id"].(string), buttons[3]["label"])
	if err != nil || snap.State != Cancelled || runs.Load() != 0 {
		t.Fatalf("cancel: %+v %v", snap, err)
	}
}

func TestStrongCannotBeConfirmedFromChat(t *testing.T) {
	c := &clock{time.Now()}
	e := engine(Strong, c)
	var runs atomic.Int32
	var chosen string
	out, _ := e.Submit(context.Background(), sendRequest(&runs, &chosen))
	if out["approval_url"] == nil || out["buttons"] != nil {
		t.Fatalf("strong response: %v", out)
	}
	c.add(time.Minute)
	if _, err := e.ConfirmChat(context.Background(), out["approval_id"].(string), "Send · XXXX"); !errors.Is(err, ErrWrongPath) {
		t.Fatalf("chat confirm of strong approval: %v", err)
	}
	if runs.Load() != 0 {
		t.Fatal("strong action executed without a panel decision")
	}
}

func TestStrongDecisionsAndWait(t *testing.T) {
	c := &clock{time.Now()}
	e := engine(Strong, c)
	var runs atomic.Int32
	var chosen string
	ctx := context.Background()

	out, _ := e.Submit(ctx, sendRequest(&runs, &chosen))
	id := out["approval_id"].(string)
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = e.DecideStrong(ctx, id, "send", true)
	}()
	snap, err := e.Wait(ctx, id, 2*time.Second)
	if err != nil || snap.State != Executed || chosen != "send" {
		t.Fatalf("wait: %+v %v", snap, err)
	}

	out2, _ := e.Submit(ctx, sendRequest(&runs, &chosen))
	snap, _ = e.DecideStrong(ctx, out2["approval_id"].(string), "", false)
	if snap.State != Denied || runs.Load() != 1 {
		t.Fatalf("deny: %+v runs=%d", snap, runs.Load())
	}
	if _, err := e.DecideStrong(ctx, out2["approval_id"].(string), "send", true); err == nil {
		t.Fatal("approved after deny")
	}
}

func TestExpiryAndHourlyLimit(t *testing.T) {
	c := &clock{time.Now()}
	e := engine(Strong, c)
	var runs atomic.Int32
	var chosen string
	ctx := context.Background()

	out, _ := e.Submit(ctx, sendRequest(&runs, &chosen))
	c.add(16 * time.Minute)
	if _, err := e.DecideStrong(ctx, out["approval_id"].(string), "send", true); err == nil {
		t.Fatal("expired approval executed")
	}

	for i := 0; i < 2; i++ {
		o, _ := e.Submit(ctx, sendRequest(&runs, &chosen))
		if _, err := e.DecideStrong(ctx, o["approval_id"].(string), "send", true); err != nil {
			t.Fatal(err)
		}
	}
	o, _ := e.Submit(ctx, sendRequest(&runs, &chosen))
	if _, err := e.DecideStrong(ctx, o["approval_id"].(string), "send", true); err == nil {
		t.Fatal("hourly limit not enforced")
	}
	c.add(61 * time.Minute)
	o, _ = e.Submit(ctx, sendRequest(&runs, &chosen))
	if _, err := e.DecideStrong(ctx, o["approval_id"].(string), "send", true); err != nil {
		t.Fatalf("limit did not reset: %v", err)
	}
}

func TestFailedExecutionIsTerminal(t *testing.T) {
	c := &clock{time.Now()}
	e := engine(Strong, c)
	out, _ := e.Submit(context.Background(), Request{Kind: "k", Options: []Option{{Key: "go", Label: "Go"}},
		Execute: func(context.Context, string) (any, error) { return nil, errors.New("smtp down") }})
	snap, _ := e.DecideStrong(context.Background(), out["approval_id"].(string), "go", true)
	if snap.State != Failed || snap.Error != "smtp down" {
		t.Fatalf("%+v", snap)
	}
}
