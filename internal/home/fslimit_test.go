package home

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func TestFsListBucketRefusesPastTheLimits(t *testing.T) {
	c := &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	l := &callLimiter{now: c.now}
	// a minute: 30 calls, one at a time, then refused
	for i := 0; i < fsListPerMinute; i++ {
		done, err := l.begin()
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		done()
	}
	if _, err := l.begin(); err != errFsListRate {
		t.Fatalf("call 31 in a minute: %v", err)
	}
	// a minute later it works again
	c.t = c.t.Add(61 * time.Second)
	done, err := l.begin()
	if err != nil {
		t.Fatalf("after a minute: %v", err)
	}
	done()

	// an hour: 20 calls a minute pass the minute limit, but the 301st call of an hour does not
	l = &callLimiter{now: c.now}
	for i := 0; i < fsListPerHour; i++ {
		done, err := l.begin()
		if err != nil {
			t.Fatalf("hourly call %d: %v", i, err)
		}
		done()
		c.t = c.t.Add(11 * time.Second) // ~5.5 a minute: under the minute limit
	}
	if _, err := l.begin(); err != errFsListRate {
		t.Fatalf("call 301 in an hour: %v", err)
	}
	c.t = c.t.Add(time.Hour)
	if done, err := l.begin(); err != nil {
		t.Fatalf("an hour later: %v", err)
	} else {
		done()
	}
}

func TestFsListAtMostTwoAtOnce(t *testing.T) {
	l := &callLimiter{}
	d1, err1 := l.begin()
	d2, err2 := l.begin()
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if _, err := l.begin(); err != errFsListBusy {
		t.Fatalf("a third at once: %v", err)
	}
	d1()
	d1() // ending twice frees one slot only
	d3, err := l.begin()
	if err != nil {
		t.Fatalf("after one ended: %v", err)
	}
	if _, err := l.begin(); err != errFsListBusy {
		t.Fatalf("still two in progress: %v", err)
	}
	d2()
	d3()
}

// The count of the last hour holds every call asked for, those refused too, and forgets what is older.
func TestFsListCounterCoversTheLastHour(t *testing.T) {
	c := &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	l := &callLimiter{now: c.now}
	for i := 0; i < fsListPerMinute+5; i++ {
		if done, err := l.begin(); err == nil {
			done()
		}
	}
	if got := l.lastHour(); got != fsListPerMinute+5 {
		t.Fatalf("counted %d", got)
	}
	c.t = c.t.Add(30 * time.Minute)
	l.begin()
	if got := l.lastHour(); got != fsListPerMinute+6 {
		t.Fatalf("after half an hour: %d", got)
	}
	c.t = c.t.Add(31 * time.Minute)
	if got := l.lastHour(); got != 1 {
		t.Fatalf("after more than an hour: %d", got)
	}
}

// Home answers fs.list through the limiter, logs one line per call with a count and no name, and shows the count in hello.
func TestFsListIsLimitedLoggedWithoutNamesAndCounted(t *testing.T) {
	dir, err := os.MkdirTemp(".", "fshome-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	abs, _ := filepath.Abs(dir)
	home, _ := filepath.EvalSymlinks(abs)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	const secretName = "very-secret-folder-name"
	os.MkdirAll(filepath.Join(home, secretName), 0o755)
	h := openHelper(t)

	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	var last any
	var lastErr error
	for i := 0; i < fsListPerMinute+3; i++ {
		last, lastErr = h.listFolders(json.RawMessage(`{"path":""}`))
		if i < fsListPerMinute && lastErr != nil {
			t.Fatalf("call %d: %v", i, lastErr)
		}
	}
	if lastErr != errFsListRate {
		t.Fatalf("the 33rd call: %v", lastErr)
	}
	_ = last
	text := logged.String()
	if n := strings.Count(text, "fs.list:"); n != fsListPerMinute+3 {
		t.Fatalf("%d log lines for %d calls:\n%s", n, fsListPerMinute+3, text)
	}
	for _, bad := range []string{secretName, home, filepath.Base(home), "path"} {
		if strings.Contains(text, bad) {
			t.Errorf("the log holds %q:\n%s", bad, text)
		}
	}
	if !strings.Contains(text, "1 folders listed") || !strings.Contains(text, "refused") {
		t.Errorf("the log lacks the count or the refusal:\n%s", text)
	}
	if got := h.hello()["fs_list_calls"]; got != fsListPerMinute+3 {
		t.Errorf("hello says %v calls", got)
	}
	// bad arguments are refused and still counted
	if _, err := h.listFolders(json.RawMessage(`{"path": 5}`)); err == nil {
		t.Error("bad arguments")
	}
}
