package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/agentbridge"
	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
)

func fakeBridgeService() {
	a, b, home := os.Getenv("FAKE_PROJECT"), os.Getenv("FAKE_OTHER"), os.Getenv("FAKE_HOME")
	if a == "" {
		a = os.Getenv("FAKE_PROJECT_API_KEY")
		b = os.Getenv("FAKE_OTHER_API_KEY")
		home = os.Getenv("FAKE_HOME_API_KEY")
	}
	var health atomic.Int32
	password := os.Getenv("OPENCODE_PASSWORD")
	if password == "" {
		password = os.Getenv("OPENCODE_SERVER_PASSWORD")
	}
	for _, p := range []string{filepath.Join(b, "marker"), filepath.Join(home, "secret")} {
		if _, err := os.ReadFile(p); err == nil {
			os.Exit(91)
		}
	}
	for _, p := range []string{filepath.Join(b, "outside"), filepath.Join(home, "outside")} {
		os.WriteFile(p, []byte("shadow"), 0600)
	}
	if os.WriteFile(filepath.Join(a, "inside"), []byte("ok"), 0600) != nil {
		os.Exit(92)
	}
	if os.Getenv("RUBI_SRT_NETWORK_TEST") == "1" {
		result, err := exec.Command("curl", "-sS", "--max-time", "5", "https://example.com").CombinedOutput()
		if err == nil || !bytes.Contains(result, []byte("403")) {
			os.Exit(93)
		}
	}
	server := &http.Server{Addr: "127.0.0.1:37173", ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NewResponseController(w).EnableFullDuplex()
		if password != "" {
			user, pass, ok := r.BasicAuth()
			if !ok || user != "opencode" || pass != password {
				w.WriteHeader(401)
				return
			}
		}
		if r.URL.Path == "/api/info" || r.URL.Path == "/global/health" {
			if health.Add(1) < 3 {
				w.WriteHeader(503)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"healthy":true,"version":"2.2.6"}`)
			return
		}
		if r.URL.Path == "/crash" {
			os.Exit(19)
		}
		if r.URL.Path == "/upgrade" {
			c, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer c.Close()
			rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
			rw.Flush()
			io.Copy(c, rw)
			return
		}
		if r.URL.Path == "/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: ready\n\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		io.Copy(w, r.Body)
	})}
	if server.ListenAndServe() != nil {
		os.Exit(94)
	}
}
func bridgeFixture(t *testing.T) (agentsandbox.Launch, agentbridge.Bootstrap, string) {
	t.Helper()
	srt, err := exec.LookPath("srt")
	if err != nil {
		t.Skip("srt unavailable")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	a, b, home := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "host-home")
	p := agentsandbox.Policy{Projects: []string{a}, Home: home, Login: filepath.Join(root, "login"), Tmp: filepath.Join(root, "tmp"), SensitiveRoots: []string{root}, AllowedDomains: []string{"github.com"}}
	settings := filepath.Join(root, "settings")
	for _, d := range []string{a, b, home, p.Login, p.Tmp, settings} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(b, "marker"), []byte("fixture"), 0600)
	os.WriteFile(filepath.Join(home, "secret"), []byte("fixture"), 0600)
	real, err := filepath.EvalSymlinks(srt)
	if err != nil {
		t.Fatal(err)
	}
	p.RuntimeRead = []string{filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(real))))}
	var search []string
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(d) {
			if st, err := os.Stat(d); err == nil && st.IsDir() {
				search = append(search, d)
				if strings.HasPrefix(d, "/workspace/") {
					p.RuntimeRead = append(p.RuntimeRead, d)
				}
			}
		}
	}
	l := agentsandbox.Launch{Policy: p, CLI: self, SRT: srt, SettingsDir: settings, SearchPath: search}
	boot := agentbridge.Bootstrap{Command: []string{self, "--bridge-fake"}, Port: 37173, Environment: map[string]string{"FAKE_PROJECT": a, "FAKE_OTHER": b, "FAKE_HOME": home, "RUBI_SRT_NETWORK_TEST": os.Getenv("RUBI_SRT_NETWORK_TEST")}}
	return l, boot, self
}
func TestInheritedPipeBridgeSRT(t *testing.T) {
	l, boot, self := bridgeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, _, err := agentbridge.Start(ctx, l, self, boot)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	password := strings.Repeat("synthetic", 5)
	frontend, err := agentbridge.Frontend(p, boot.Port, password)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer frontend.Close()
	go frontend.Serve(ln)
	base := "http://" + ln.Addr().String()
	hc := &http.Client{Timeout: 5 * time.Second}
	response, err := hc.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("unauthenticated ingress accepted")
	}
	request := func(path, body string) *http.Request {
		r, err := http.NewRequestWithContext(ctx, "POST", base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.SetBasicAuth("opencode", password)
		return r
	}
	direct := &http.Client{Transport: p, Timeout: 5 * time.Second}
	dr, _ := http.NewRequest("POST", "http://127.0.0.1:37173/echo", strings.NewReader(strings.Repeat("x", 300000)))
	dresp, derr := direct.Do(dr)
	if derr != nil {
		t.Fatalf("direct transport failed: %v mux=%v", derr, p.Err())
	}
	dbody, derr := io.ReadAll(dresp.Body)
	dresp.Body.Close()
	if derr != nil || len(dbody) != 300000 {
		t.Fatalf("direct body: len=%d err=%v mux=%v diagnostic=%s", len(dbody), derr, p.Err(), p.Diagnostic())
	}
	body := strings.Repeat("framed-bytes", 200000)
	response, err = hc.Do(request("/echo", body))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(got) != body {
		t.Fatalf("large binary stream corrupted: got=%d err=%v channel=%v alive=%v", len(got), err, p.Err(), p.Alive())
	}
	// An SSE stream must flush before completion, coexist with other streams,
	// and keep the process pinned until cancellation/body close.
	events, err := hc.Do(request("/events", ""))
	if err != nil {
		t.Fatal(err)
	}
	event := make([]byte, len("data: ready\n\n"))
	if _, err := io.ReadFull(events.Body, event); err != nil {
		t.Fatal(err)
	}
	if p.StopIdle(time.Now().Add(time.Hour), time.Second) {
		t.Fatal("idle cleanup killed active SSE")
	}
	response, err = hc.Do(request("/echo", "concurrent"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	events.Body.Close()
	// A raw Upgrade is forwarded, including its bidirectional byte stream.
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := request("/upgrade", "")
	r.Method = "GET"
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "test")
	if err = r.Write(conn); err != nil {
		t.Fatal(err)
	}
	// Read exactly the headers; do not drop bytes from an upgraded stream.
	var headers []byte
	one := make([]byte, 1)
	for !bytes.HasSuffix(headers, []byte("\r\n\r\n")) {
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err = io.ReadFull(conn, one); err != nil {
			t.Fatal(err)
		}
		headers = append(headers, one[0])
	}
	if !bytes.Contains(headers, []byte("101 Switching")) {
		t.Fatal("upgrade refused")
	}
	conn.Write([]byte("echo"))
	echo := make([]byte, 4)
	if _, err = io.ReadFull(conn, echo); err != nil || string(echo) != "echo" {
		t.Fatal("upgrade bytes lost", err)
	}
	conn.Close()
	for _, path := range []string{filepath.Join(boot.Environment["FAKE_OTHER"], "outside"), filepath.Join(l.Policy.Home, "outside")} {
		if _, err = os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("sandbox wrote outside project")
		}
	}
	// HTTP frontend must not become reachable through another project's sandbox.
	b := l
	b.Policy.Projects = []string{boot.Environment["FAKE_OTHER"]}
	root := filepath.Dir(l.SettingsDir)
	b.Policy.Login = filepath.Join(root, "b-login")
	b.Policy.Tmp = filepath.Join(root, "b-tmp")
	b.SettingsDir = filepath.Join(root, "b-settings")
	for _, d := range []string{b.Policy.Login, b.Policy.Tmp, b.SettingsDir} {
		os.Mkdir(d, 0700)
	}
	b.CLI = "/bin/sh"
	b.Args = []string{"-c", `curl -s --max-time 2 "$1"; test "$?" -ne 0`, "sh", base}
	var out, stderr bytes.Buffer
	code, err := agentsandbox.Run(ctx, b, nil, &out, &stderr)
	if err != nil || code != 0 || out.Len() != 0 {
		t.Fatalf("other-project ingress reachable: code=%d err=%v", code, err)
	}
	// Crash closes all streams, exits the relay and reaps its confined process.
	_, _ = hc.Do(request("/crash", ""))
	select {
	case <-p.Done():
	case <-ctx.Done():
		t.Fatal("crashed backend leaked relay")
	}
	if p.Alive() {
		t.Fatal("crashed service still reusable")
	}
}
func TestBridgeIdleAndRefusalSRT(t *testing.T) {
	l, boot, self := bridgeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p, _, err := agentbridge.Start(ctx, l, self, boot)
	if err != nil {
		t.Fatal(err)
	}
	if !p.StopIdle(time.Now().Add(time.Minute), time.Second) || p.Alive() {
		t.Fatal("idle backend was not stopped")
	}
	l.Policy.Projects = nil
	if p, _, err := agentbridge.Start(ctx, l, self, boot); err == nil {
		p.Close()
		t.Fatal("missing project accepted")
	}
	// Direct invocation outside srt must never spawn the service.
	var in, out bytes.Buffer
	// An incomplete control envelope is a refusal, not an exec fallback.
	_ = fmt.Sprintf("%v", boot)
	if agentbridge.Relay(io.NopCloser(&in), nopWriteCloser{&out}) == nil {
		t.Fatal("relay accepted absent supervisor")
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// Exercise the real serve entrypoint and credential pipe with both supported
// upstream password contracts, using a behaviorally faithful HTTP CLI fixture.
func TestOpenCodeServePasswordContractsSRT(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			l, b, self := bridgeFixture(t)
			root := filepath.Dir(l.SettingsDir)
			wrapper := filepath.Join(root, "opencode-fixture")
			if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec '"+self+"' --bridge-fake \"$@\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			l.CLI = wrapper
			l.Args = []string{"serve", "--hostname=0.0.0.0", "--port", "1"}
			l.Policy.RuntimeRead = append(l.Policy.RuntimeRead, wrapper)
			values := map[string]string{"FAKE_PROJECT_API_KEY": b.Environment["FAKE_PROJECT"], "FAKE_OTHER_API_KEY": b.Environment["FAKE_OTHER"], "FAKE_HOME_API_KEY": b.Environment["FAKE_HOME"]}
			data, _ := json.Marshal(values)
			l.CredentialsFile = filepath.Join(root, "credentials.json")
			os.WriteFile(l.CredentialsFile, data, 0600)
			password := strings.Repeat("synthetic-selected-", 3)
			t.Setenv("OPENCODE_PASSWORD", "")
			t.Setenv("OPENCODE_SERVER_PASSWORD", password)
			path := "/global/health"
			if version == "v2" {
				t.Setenv("OPENCODE_PASSWORD", password)
				t.Setenv("OPENCODE_SERVER_PASSWORD", strings.Repeat("synthetic-wrong-", 3))
				path = "/api/info"
			}
			in, out, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			defer out.Close()
			original := os.Stdout
			os.Stdout = out
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			done := make(chan error, 1)
			go func() {
				code, err := serveOpenCode(ctx, l)
				if code != 0 && err == nil {
					err = fmt.Errorf("unexpected exit %d", code)
				}
				done <- err
			}()
			defer func() {
				cancel()
				err := <-done
				os.Stdout = original
				if err != nil {
					t.Error(err)
				}
			}()
			discovery := make(chan string, 1)
			go func() { line, _ := bufio.NewReader(in).ReadString('\n'); discovery <- line }()
			var line string
			select {
			case line = <-discovery:
			case <-ctx.Done():
				t.Fatal("discovery did not arrive")
			}
			if strings.Contains(line, password) || !strings.HasPrefix(line, "server listening on http://127.0.0.1:") {
				t.Fatal("unsafe discovery output")
			}
			base := strings.TrimSpace(strings.TrimPrefix(line, "server listening on "))
			hc := &http.Client{Timeout: 3 * time.Second}
			resp, err := hc.Get(base + path)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Fatal("unauthenticated ingress accepted")
			}
			req, _ := http.NewRequest("GET", base+path, nil)
			req.SetBasicAuth("opencode", password)
			resp, err = hc.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatal("selected password not passed to native service")
			}
		})
	}
}
func TestOpenCodeRefusesMissingPassword(t *testing.T) {
	t.Setenv("OPENCODE_PASSWORD", "")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "")
	if code, err := serveOpenCode(context.Background(), agentsandbox.Launch{}); code != 125 || err == nil {
		t.Fatal("unauthenticated bridge accepted")
	}
}
