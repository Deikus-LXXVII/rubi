package agent

import (
	"bytes"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLooksSecret(t *testing.T) {
	for _, s := range []string{
		"x-api-key: sk-ant-api03-abc", "Authorization: Bearer abc", "GET /cb?code=abc", "refresh it", "my Token",
		"a1B2c3D4e5F6g7H8i9J0k1L2m3N4o5P6", "abcd.efgh_ijkl-mnop#qrst~uvwx+yz0123456789==",
		"1a2b3c4d.5e6f7g8h-9i0j1k2l_3m4n5o6p7q8r9",
	} {
		if !LooksSecret(s) {
			t.Errorf("%q passed", s)
		}
	}
	for _, s := range []string{"npm error network request failed", "Downloaded 31 of 148 MB", "#5 [builder 2/4] RUN npm ci --ignore-scripts"} {
		if LooksSecret(s) {
			t.Errorf("%q dropped", s)
		}
	}
}

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// Only the runner's stderr is logged, and never a line that looks secret; any other exec's is dropped.
func TestSpawnStderr(t *testing.T) {
	var buf safeBuf
	log.SetOutput(&buf)
	defer log.SetOutput(io.Discard)
	script := "echo visible-line >&2; echo 'oops sk-ant-oat01-zzz' >&2; echo 'a refresh_token=abc' >&2; echo out"
	for _, logged := range []bool{true, false} {
		c, err := spawnProcess([]string{"/bin/sh", "-c", script}, logged)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(c)
		<-c.Done()
		if strings.TrimSpace(string(out)) != "out" {
			t.Errorf("stdout %q", out)
		}
		time.Sleep(150 * time.Millisecond)
		got := buf.String()
		if strings.Contains(got, "sk-ant") || strings.Contains(got, "refresh_token") {
			t.Errorf("logged=%v: a secret reached the log: %q", logged, got)
		}
		if logged != strings.Contains(got, "visible-line") {
			t.Errorf("logged=%v: log %q", logged, got)
		}
		buf.mu.Lock()
		buf.b.Reset()
		buf.mu.Unlock()
	}
}
