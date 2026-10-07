package icloudmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
)

const (
	testUser = "me@icloud.com"
	testPass = "abcd-efgh-ijkl-mnop"
)

// startIMAP runs an in-memory IMAP server shaped like iCloud (special-use Drafts and "Sent Messages").
func startIMAP(t *testing.T) string {
	t.Helper()
	mem := imapmemserver.New()
	u := imapmemserver.NewUser(testUser, testPass)
	_ = u.Create("INBOX", nil)
	_ = u.Create("Drafts", &imap.CreateOptions{SpecialUse: []imap.MailboxAttr{imap.MailboxAttrDrafts}})
	_ = u.Create("Sent Messages", &imap.CreateOptions{SpecialUse: []imap.MailboxAttr{imap.MailboxAttrSent}})
	mem.AddUser(u)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	dialIMAP = func(addr string) (*imapclient.Client, error) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		return imapclient.New(conn, nil), nil
	}
	return ln.Addr().String()
}

// deliver appends a raw message to a folder, as if it arrived from outside.
func deliver(t *testing.T, addr, box, raw string, flags ...imap.Flag) {
	t.Helper()
	c, err := dialIMAP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Login(testUser, testPass).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := appendMessage(c, box, flags, []byte(strings.ReplaceAll(raw, "\n", "\r\n"))); err != nil {
		t.Fatal(err)
	}
}

type sentMail struct {
	from string
	to   []string
	raw  string
}

// fakeHost implements integrations.Host. Approvals are decided by `choose`.
type fakeHost struct {
	mu       sync.Mutex
	settings json.RawMessage
	secrets  map[string]string
	state    json.RawMessage
	levels   map[string]approvals.Level
	choose   string // option picked when an approval is requested; "" = leave pending
	events   []map[string]any
	pending  []approvals.Request
}

func (h *fakeHost) ID() string { return ID }
func (h *fakeHost) Settings(v any) error {
	if h.settings == nil {
		return errors.New("not set up")
	}
	return json.Unmarshal(h.settings, v)
}
func (h *fakeHost) Secret(k string) (string, error) { return h.secrets[k], nil }
func (h *fakeHost) Level(kind string) approvals.Level {
	if l, ok := h.levels[kind]; ok {
		return l
	}
	return approvals.None
}
func (h *fakeHost) Submit(ctx context.Context, req approvals.Request) (map[string]any, error) {
	h.pending = append(h.pending, req)
	if h.choose == "" {
		return map[string]any{"status": "awaiting_approval"}, nil
	}
	res, err := req.Execute(ctx, h.choose)
	return map[string]any{"status": "done", "result": res}, err
}
func (h *fakeHost) Emit(typ string, data map[string]any, _ []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, map[string]any{"type": typ, "data": data})
}
func (h *fakeHost) Audit(string, map[string]any) {}
func (h *fakeHost) LoadState(v any) error {
	if h.state == nil {
		return nil
	}
	return json.Unmarshal(h.state, v)
}
func (h *fakeHost) SaveState(v any) error {
	b, err := json.Marshal(v)
	h.state = b
	return err
}
func (h *fakeHost) Logf(string, ...any) {}

func setup(t *testing.T) (*integration, *fakeHost, string, *[]sentMail) {
	t.Helper()
	addr := startIMAP(t)
	defaultIMAPAddr = addr
	x := &integration{}
	fields := map[string]string{"address": " Me@iCloud.com ", "from_name": "Test User"}
	if _, _, err := x.Validate(context.Background(), fields, map[string]string{"app_password": "wrong"}); !errors.Is(err, errAuth) {
		t.Fatalf("wrong password: %v", err)
	}
	secrets := map[string]string{"app_password": " " + testPass + " "}
	raw, account, err := x.Validate(context.Background(), fields, secrets)
	if err != nil || account != testUser || secrets["app_password"] != testPass {
		t.Fatalf("validate: %v account=%q", err, account)
	}
	var s Settings
	_ = json.Unmarshal(raw, &s)
	if s.Drafts != "Drafts" || s.Sent != "Sent Messages" || s.IMAPAddr != addr {
		t.Fatalf("settings: %+v", s)
	}
	var sent []sentMail
	sendMail = func(_, user, pw, from string, to []string, msg []byte) error {
		if user != testUser || pw != testPass {
			return errAuth
		}
		sent = append(sent, sentMail{from: from, to: to, raw: string(msg)})
		return nil
	}
	return x, &fakeHost{settings: raw, secrets: map[string]string{"app_password": testPass}}, addr, &sent
}

func TestReadDoesNotMarkSeen(t *testing.T) {
	_, h, addr, _ := setup(t)
	deliver(t, addr, "INBOX", "From: Anna <anna@example.com>\nTo: me@icloud.com\nSubject: =?utf-8?q?=D0=92=D1=81=D1=82=D1=80=D0=B5=D1=87=D0=B0?=\nMessage-ID: <a1@example.com>\nDate: Wed, 07 Oct 2026 10:00:00 +0000\nContent-Type: text/plain; charset=utf-8\n\nПривет! В 15:00?\n")
	deliver(t, addr, "INBOX", "From: shop@example.com\nTo: me@icloud.com\nSubject: Sale\nMessage-ID: <s1@example.com>\nContent-Type: text/html\n\n<html><head><style>x{}</style></head><body><p>Big <b>sale</b></p><script>steal()</script></body></html>\n")

	c, _, err := session(h)
	if err != nil {
		t.Fatal(err)
	}
	defer logout(c)
	msgs, err := search(c, searchQuery{Subject: "Встреча"})
	if err != nil || len(msgs) != 1 || msgs[0].From != "Anna <anna@example.com>" || msgs[0].Seen {
		t.Fatalf("search: %+v %v", msgs, err)
	}
	raw, err := fetchRaw(c, "INBOX", msgs[0].UID)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := parseMessage(raw, 0)
	if m.Subject != "Встреча" || !strings.Contains(m.Text, "Привет") || m.MessageID != "<a1@example.com>" {
		t.Fatalf("parsed: %+v", m)
	}
	after, _ := search(c, searchQuery{Subject: "Встреча"})
	if after[0].Seen {
		t.Fatal("reading marked the message as read")
	}
	all, _ := search(c, searchQuery{})
	raw2, _ := fetchRaw(c, "INBOX", all[0].UID) // newest first: the HTML one
	m2, _ := parseMessage(raw2, 0)
	if strings.Contains(m2.Text, "steal") || !strings.Contains(m2.Text, "Big sale") {
		t.Fatalf("html to text: %q", m2.Text)
	}
}

func TestComposeHeaders(t *testing.T) {
	s := defaultSettings()
	s.Address, s.FromName = testUser, "Тест Тестович"
	m, err := compose(s, draft{To: []string{"Anna <anna@example.com>"}, Bcc: []string{"hidden@example.com"},
		Subject: "Встреча", Body: "line1\nline2 — ok"}, "<a1@example.com>", "<a0@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	raw := string(m.raw)
	for _, want := range []string{"In-Reply-To: <a1@example.com>", "References: <a0@example.com> <a1@example.com>",
		"Subject: =?utf-8?q?Re:_", "line1\r\nline2"} {
		if !strings.Contains(raw, want) {
			t.Errorf("missing %q in:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "hidden@example.com") {
		t.Error("Bcc leaked into headers")
	}
	if len(m.envelope) != 2 || m.envelope[1] != "hidden@example.com" {
		t.Errorf("envelope: %v", m.envelope)
	}
	if _, err := compose(s, draft{To: []string{"not an address"}}, "", ""); err == nil {
		t.Error("invalid address accepted")
	}
}

func TestSendGatedTrackedAndReplyDetected(t *testing.T) {
	x, h, addr, sent := setup(t)
	h.levels = map[string]approvals.Level{kindSend: approvals.Strong}

	c, s, _ := session(h)
	msg, err := composeReply(c, s, draft{To: []string{"anna@example.com"}, Subject: "Meeting", Body: "15:00?"})
	logout(c)
	if err != nil {
		t.Fatal(err)
	}

	// Pending approval: nothing is sent.
	if _, err := h.Submit(context.Background(), approvals.Request{Kind: kindSend, Options: sendOptions,
		Execute: func(_ context.Context, o string) (any, error) { return x.send(h, msg, o == "send_track", 14) }}); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 0 {
		t.Fatal("sent before approval")
	}

	// The user picks "Send and notify on reply".
	res, err := x.send(h, msg, true, 14)
	if err != nil {
		t.Fatal(err)
	}
	out := res.(map[string]any)
	if len(*sent) != 1 || out["saved_to_sent"] != true || out["tracking"] == nil {
		t.Fatalf("send result: %+v sent=%d", out, len(*sent))
	}

	// Nothing arrived yet.
	if err := x.poll(h); err != nil || len(h.events) != 0 {
		t.Fatalf("poll: %v events=%v", err, h.events)
	}

	// A threaded reply, an unrelated mail with the same subject, our own copy, and a reply without headers.
	deliver(t, addr, "INBOX", fmt.Sprintf("From: Anna <anna@example.com>\nTo: me@icloud.com\nSubject: Re: Meeting\nMessage-ID: <r1@example.com>\nIn-Reply-To: %s\nDate: %s\n\nYes!\n", msg.messageID, time.Now().Format(time.RFC1123Z)))
	deliver(t, addr, "INBOX", "From: spam@example.com\nTo: me@icloud.com\nSubject: Re: Meeting\nMessage-ID: <x1@example.com>\n\nbuy\n")
	deliver(t, addr, "INBOX", "From: me@icloud.com\nTo: anna@example.com\nSubject: Re: Meeting\nMessage-ID: <own@icloud.com>\n\nmine\n")
	if err := x.poll(h); err != nil {
		t.Fatal(err)
	}
	if len(h.events) != 1 {
		t.Fatalf("want 1 reply event, got %v", h.events)
	}
	reply := h.events[0]["data"].(map[string]any)["reply"].(map[string]any)
	if reply["match"] != "headers" || !strings.Contains(reply["from"].(string), "anna@example.com") {
		t.Fatalf("reply: %v", reply)
	}

	deliver(t, addr, "INBOX", "From: anna@example.com\nTo: me@icloud.com\nSubject: Ответ: Re: Meeting\nMessage-ID: <r2@example.com>\n\nalso yes\n")
	if err := x.poll(h); err != nil {
		t.Fatal(err)
	}
	if len(h.events) != 2 || h.events[1]["data"].(map[string]any)["reply"].(map[string]any)["match"] != "subject" {
		t.Fatalf("subject heuristic: %v", h.events)
	}
	if err := x.poll(h); err != nil || len(h.events) != 2 {
		t.Fatalf("duplicates after re-poll: %v", h.events)
	}

	// Stop tracking: nothing more is reported and iCloud isn't contacted.
	st, _ := x.loadState(h)
	if ok, _ := x.stopTracking(h, st.Tracked[0].ID); !ok {
		t.Fatal("stop tracking")
	}
	deliver(t, addr, "INBOX", fmt.Sprintf("From: anna@example.com\nSubject: Re: Meeting\nMessage-ID: <r3@example.com>\nIn-Reply-To: %s\n\nthird\n", msg.messageID))
	if err := x.poll(h); err != nil || len(h.events) != 2 {
		t.Fatalf("reported after stop: %v", h.events)
	}
}

func TestNormalizeSubject(t *testing.T) {
	for in, want := range map[string]string{"Re: RE:  Fwd: Meeting": "meeting", "Ответ: Встреча": "встреча", "AW[2]: x": "x"} {
		if got := normSubject(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}
