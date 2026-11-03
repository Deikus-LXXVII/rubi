package t3

// Contract tests against the real pinned T3. They run only with RUBI_T3_CONTRACT=1:
//
//	RUBI_T3_BIN     the pinned t3 bin (a JS file)
//	RUBI_T3_NODE    the node binary
//	RUBI_T3_CLAUDE  an unmodified claude binary (only for the run part)
//	RUBI_T3_UPDATE  1 rewrites t3/contract/tools.json instead of comparing
//
// The run part points claude at the scripted Messages API stand-in (runner/test/support/mock.mjs) with a
// throwaway config folder, so no account is used.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	rt3 "github.com/Deikus-LXXVII/rubi/t3"
)

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

type contractEnv struct {
	server *Server
	client *Client
	tok    Token
}

func contractServer(t *testing.T) *contractEnv {
	t.Helper()
	if os.Getenv("RUBI_T3_CONTRACT") != "1" {
		t.Skip("set RUBI_T3_CONTRACT=1 (with RUBI_T3_BIN and RUBI_T3_NODE) to run the T3 contract tests")
	}
	bin, node := os.Getenv("RUBI_T3_BIN"), os.Getenv("RUBI_T3_NODE")
	if bin == "" || node == "" {
		t.Skip("RUBI_T3_BIN and RUBI_T3_NODE are needed")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	s := &Server{Node: node, Bin: bin, BaseDir: t.TempDir(), Port: port}
	t.Cleanup(s.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	v, err := s.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pin struct{ Version string }
	if err := json.Unmarshal(rt3.Upstream, &pin); err != nil {
		t.Fatal(err)
	}
	if v != pin.Version {
		t.Fatalf("server version %q is not the pin %q", v, pin.Version)
	}
	access := "full-access"
	code, err := s.PairingCode(ctx, PairingScopes(access), 10*time.Minute, "rubi contract test")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := Pair(ctx, http.DefaultClient, s.Base(), code, access, "Rubi contract test")
	if err != nil {
		t.Fatal(err)
	}
	want := PairingScopes(access)
	got := append([]string(nil), tok.Scopes...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("token scopes = %v, want %v", got, want)
	}
	c := New(s.Base(), tok, FullAccess)
	t.Cleanup(func() { _ = c.Close() })
	return &contractEnv{s, c, tok}
}

func TestContractToolSchemas(t *testing.T) {
	e := contractServer(t)
	tools, err := e.client.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]any{}
	for _, tl := range tools {
		// Through JSON, so both sides compare as plain values.
		b, _ := json.Marshal(tl.InputSchema)
		var v any
		_ = json.Unmarshal(b, &v)
		have[tl.Name] = v
	}
	got := map[string]any{}
	for _, n := range allTools {
		v, ok := have[n]
		if !ok {
			t.Errorf("T3 has no tool %s", n)
			continue
		}
		got[n] = v
	}
	path := filepath.Join("..", "..", "..", "t3", "contract", "tools.json")
	if os.Getenv("RUBI_T3_UPDATE") == "1" {
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := rt3.Contract.ReadFile("contract/tools.json")
	if err != nil {
		t.Fatal(err)
	}
	var snap map[string]any
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	for _, n := range allTools {
		if !reflect.DeepEqual(snap[n], got[n]) {
			t.Errorf("the input schema of %s changed; if intended, rerun with RUBI_T3_UPDATE=1 and review the diff\nsnapshot: %s\nserver:   %s", n, mustJSON(snap[n]), mustJSON(got[n]))
		}
	}
}

// startMock runs the scripted Messages API stand-in and returns its URL.
func startMock(t *testing.T, node string) string {
	t.Helper()
	urlFile := filepath.Join(t.TempDir(), "mock.url")
	cmd := exec.Command(node, filepath.Join("..", "..", "..", "t3", "contract", "mockserve.mjs"), urlFile)
	cmd.Env = []string{"PATH=" + filepath.Dir(node) + ":/usr/bin:/bin"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(urlFile); err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the mock did not start")
	return ""
}

// addInstance writes a Claude provider instance backed by the mock into T3's settings, which T3 watches.
func addInstance(t *testing.T, s *Server, id, claude, mockURL string) {
	t.Helper()
	path := filepath.Join(s.BaseDir, "userdata", "settings.json")
	settings := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &settings)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	inst, _ := settings["providerInstances"].(map[string]any)
	if inst == nil {
		inst = map[string]any{}
	}
	inst[id] = map[string]any{
		"driver": "claudeAgent", "displayName": "Claude (mock)", "enabled": true,
		"environment": []map[string]any{
			{"name": "ANTHROPIC_BASE_URL", "value": mockURL},
			{"name": "ANTHROPIC_API_KEY", "value": "sk-ant-mock-not-a-key"},
		},
		"config": map[string]any{"binaryPath": claude, "homePath": t.TempDir()},
	}
	settings["providerInstances"] = inst
	b, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func lastAssistant(items []Item) string {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Type == "assistant_message" {
			return items[i].Text
		}
	}
	return ""
}

func TestContractRun(t *testing.T) {
	e := contractServer(t)
	claude := os.Getenv("RUBI_T3_CLAUDE")
	if claude == "" {
		t.Skip("RUBI_T3_CLAUDE is needed for the run part")
	}
	const instance = "claude-mock"
	addInstance(t, e.server, instance, claude, startMock(t, e.server.Node))
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	c := e.client

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.CreateProject(ctx, root, "contract")
	if err != nil {
		t.Fatal(err)
	}

	var run Run
	in := LaunchIn{ProjectID: p.ID, Title: "hello", Instance: instance, Model: "claude-sonnet-4-6", Mode: Auto,
		Message: "MOCKSCRIPT:hello say hello", RequestID: "launch-1"}
	// The settings file is watched, so the instance may need a moment; the request id makes a retry safe.
	for i := 0; ; i++ {
		run, err = c.Launch(ctx, in)
		if err == nil || i >= 20 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	waitDone := func(threadID string) {
		t.Helper()
		for i := 0; i < 20; i++ {
			w, err := c.Wait(ctx, threadID, 15*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if w.Done() {
				if w.Status != "completed" {
					t.Fatalf("run ended %s", w.Status)
				}
				return
			}
		}
		t.Fatal("run did not end")
	}
	waitDone(run.ThreadID)
	th, items, err := c.Read(ctx, run.ThreadID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if th.ProjectID != p.ID || lastAssistant(items) != "MOCK-OK hello" {
		t.Fatalf("thread %+v, items %+v", th, items)
	}

	// Two sends with one request id make one message.
	count := func(items []Item) int {
		n := 0
		for _, it := range items {
			if it.Type == "user_message" {
				n++
			}
		}
		return n
	}
	before := count(items)
	r1, err := c.Send(ctx, run.ThreadID, "second turn", "send-1")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := c.Send(ctx, run.ThreadID, "second turn", "send-1")
	if err != nil {
		t.Fatal(err)
	}
	if r1.MessageID == "" || r1.MessageID != r2.MessageID {
		t.Errorf("message ids %q and %q for one request id", r1.MessageID, r2.MessageID)
	}
	waitDone(run.ThreadID)
	_, items, err = c.Read(ctx, run.ThreadID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := count(items) - before; n != 1 {
		t.Errorf("%d user messages added by two sends with one request id", n)
	}

	// A fork carries the history: the mock answers "done (hello)" only if it still sees MOCKSCRIPT:hello.
	fork, err := c.Fork(ctx, run.ThreadID, "fork")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(ctx, fork, "and in the fork?", "send-fork"); err != nil {
		t.Fatal(err)
	}
	waitDone(fork)
	_, items, err = c.Read(ctx, fork, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := lastAssistant(items); !strings.Contains(got, "done (hello)") {
		t.Errorf("the fork's reply = %q; the history did not carry over", got)
	}
}
