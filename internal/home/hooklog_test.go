package home

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLog sends the standard logger to a buffer until the test ends.
func captureLog(t *testing.T) *lockedBuf {
	t.Helper()
	buf := &lockedBuf{}
	old, flags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(old); log.SetFlags(flags) })
	return buf
}

// hookLines are the hook lines of the log.
func hookLines(buf *lockedBuf) []string {
	var out []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(l, "claude-hook ") {
			out = append(out, l)
		}
	}
	return out
}

// wantHookLine waits for a line matching the pattern (anchored, with the "<n>ms" end).
func wantHookLine(t *testing.T, buf *lockedBuf, pattern string) string {
	t.Helper()
	re := regexp.MustCompile(`^claude-hook ` + pattern + ` \d+ms$`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, l := range hookLines(buf) {
			if re.MatchString(l) {
				return l
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no hook line matching %q in:\n%s", pattern, buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func noSecrets(t *testing.T, buf *lockedBuf, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(buf.String(), s) {
			t.Fatalf("the log carries %q:\n%s", s, buf.String())
		}
	}
}

// sockRoundTrip sends one line to the socket and returns the reply line.
func sockRoundTrip(t *testing.T, sock string, line []byte) string {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

func TestEventHooksAreLogged(t *testing.T) {
	buf := captureLog(t)
	const sid = "abcdefgh-1234-5678"
	c := liveClaude(t)
	sock := serveSocket(t, c)

	sockRoundTrip(t, sock, hook("SessionStart", sid, map[string]any{"source": "startup"}))
	wantHookLine(t, buf, `SessionStart session=abcdefgh outcome=nothing`)

	if _, _, err := c.Send(sid, "Vega", "SECRETMESSAGE for the session"); err != nil {
		t.Fatal(err)
	}
	sockRoundTrip(t, sock, hook("UserPromptSubmit", sid, nil))
	wantHookLine(t, buf, `UserPromptSubmit session=abcdefgh outcome=delivered 1`)

	if _, _, err := c.Send(sid, "Vega", "SECRETSECOND"); err != nil {
		t.Fatal(err)
	}
	sockRoundTrip(t, sock, hook("Stop", sid, map[string]any{"last_assistant_message": "SECRETANSWER\n```rubi-ask\nSECRETASKTEXT\n```"}))
	l := wantHookLine(t, buf, `Stop session=abcdefgh outcome=(replies 1 )?(asks 1 )?blocked`)
	if !strings.Contains(l, "replies 1") {
		t.Fatalf("Stop line: %s", l)
	}
	sockRoundTrip(t, sock, hook("SessionEnd", sid, nil))
	wantHookLine(t, buf, `SessionEnd session=abcdefgh outcome=nothing`)
	noSecrets(t, buf, "SECRETMESSAGE", "SECRETSECOND", "SECRETANSWER", "SECRETASKTEXT", "/work", "Vega")
}

func TestHookRefusedByProtocolIsLogged(t *testing.T) {
	buf := captureLog(t)
	c := liveClaude(t)
	sock := serveSocket(t, c)
	line := hook("Stop", "sess-proto-1", map[string]any{"rubi_hook_protocol": 99})
	sockRoundTrip(t, sock, line)
	wantHookLine(t, buf, `Stop session=sess-pro outcome=refused \(protocol\)`)
}

func TestEventSlotsFullIsLogged(t *testing.T) {
	c := liveClaude(t)
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	if r, outcome := c.event(hook("Stop", "s1", nil), slots); r != (HookReply{}) || outcome != "slots full" {
		t.Fatalf("%+v %q", r, outcome)
	}
}

func TestPermissionRequestLogsIdAndDigestOnly(t *testing.T) {
	buf := captureLog(t)
	c := &Claude{}
	sock := serveSocket(t, c)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reqs := poller(ctx, c)

	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(hookJSON)); err != nil {
		t.Fatal(err)
	}
	reply := make(chan string, 1)
	go func() { reply <- sockRoundTrip(t, sock, compact.Bytes()) }()
	r := <-reqs
	if err := c.Decide(answer(r, "allow", "")); err != nil {
		t.Fatal(err)
	}
	<-reply
	l := wantHookLine(t, buf, `PermissionRequest session=sess-1 outcome=allowed req=`+regexp.QuoteMeta(r.ID)+` digest=`+regexp.QuoteMeta(r.Digest[:8]))
	if strings.Contains(l, r.Digest) {
		t.Fatalf("the whole digest is logged: %s", l)
	}
	noSecrets(t, buf, "git push", "origin main", "Push", "/work")
}

func TestWaiterIsLogged(t *testing.T) {
	buf := captureLog(t)
	c := liveClaude(t, "waiter-session-1")
	idle(c, "waiter-session-1")
	sock := serveSocket(t, c)
	w := startWaiter(sock, "waiter-session-1", time.Minute)
	attached(t, c, "waiter-session-1")
	wantHookLine(t, buf, `RubiWait session=waiter-s outcome=attach`)
	if _, _, err := c.Send("waiter-session-1", "Vega", "SECRETWAKETEXT"); err != nil {
		t.Fatal(err)
	}
	gotResult(t, w)
	wantHookLine(t, buf, `RubiWait session=waiter-s outcome=woke 1`)
	wantHookLine(t, buf, `RubiWait session=waiter-s outcome=close \(wake done\)`)
	noSecrets(t, buf, "SECRETWAKETEXT", "Vega")
}

func TestToolCallsAreLogged(t *testing.T) {
	buf := captureLog(t)
	c := liveClaude(t, "tool-session-1")
	c.Hook(cwdHook("UserPromptSubmit", "tool-session-1", "/work", nil))
	e := newToolEnv(t, c, "tool-session-1", "/work")
	e.ok("rubi_ask", map[string]any{"text": "SECRETASKQUESTION"})
	wantHookLine(t, buf, `\S+:rubi_ask session=tool-ses outcome=ok`)
	e.fails("rubi_ask", map[string]any{"text": "  "}, "empty")
	wantHookLine(t, buf, `\S+:rubi_ask session=tool-ses outcome=error`)
	noSecrets(t, buf, "SECRETASKQUESTION", "/work")
}

func TestHookTokenKeepsTheLineOneSafeWord(t *testing.T) {
	if got := hookToken("a b\nc\x00d", 32); got != "a?b?c?d" {
		t.Fatalf("%q", got)
	}
	if got := hookToken("0123456789", 8); got != "01234567" {
		t.Fatalf("%q", got)
	}
	if got := hookToken("", 8); got != "-" {
		t.Fatalf("%q", got)
	}
}
