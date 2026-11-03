package t3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func keys(m map[string]any) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

func TestLaunchClampsToTheCeiling(t *testing.T) {
	f := newFake(t, allTools...)
	launchedMode := ""
	f.reply["t3_thread_launch"] = func(a map[string]any) (any, string) {
		launchedMode, _ = a["runtimeMode"].(string)
		return map[string]any{"threadId": "th1", "link": "l", "projectId": "p1", "runId": "r1", "status": "running"}, ""
	}
	f.reply["t3_thread_read"] = func(map[string]any) (any, string) {
		return map[string]any{"thread": map[string]any{"threadId": "th1", "runtimeMode": launchedMode}}, ""
	}
	c := f.client(ApprovalRequired)
	run, err := c.Launch(context.Background(), LaunchIn{ProjectID: "p1", Title: "t", Instance: "i", Model: "m",
		Mode: FullAccess, Message: "hi", RequestID: "req-1"})
	if err != nil {
		t.Fatal(err)
	}
	if run.ThreadID != "th1" || run.RunID != "r1" || run.Status != "running" {
		t.Errorf("run = %+v", run)
	}
	got := f.lastOf("t3_thread_launch")
	if got.Args["runtimeMode"] != "approval-required" {
		t.Errorf("sent runtimeMode %v under an approval-required ceiling", got.Args["runtimeMode"])
	}
	want := []string{"clientRequestId", "message", "modelSelection", "projectId", "runtimeMode", "title"}
	if !reflect.DeepEqual(keys(got.Args), want) {
		t.Errorf("keys = %v", keys(got.Args))
	}
	if !reflect.DeepEqual(got.Args["modelSelection"], map[string]any{"instanceId": "i", "model": "m"}) {
		t.Errorf("modelSelection = %v", got.Args["modelSelection"])
	}
	if got.Header.Get("t3-mode-limit") != "approval-required/default" || got.Header.Get("Authorization") != "Bearer "+fakeToken {
		t.Errorf("headers = %v", got.Header)
	}

	// A lower request stays lower under a higher ceiling.
	c2 := f.client(FullAccess)
	if _, err := c2.Launch(context.Background(), LaunchIn{Mode: Auto, RequestID: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := f.lastOf("t3_thread_launch"); got.Args["runtimeMode"] != "auto" || got.Header.Get("t3-mode-limit") != "full-access/default" {
		t.Errorf("auto under full-access: %v %v", got.Args["runtimeMode"], got.Header)
	}

	n := f.nCalls()
	if _, err := c.Launch(context.Background(), LaunchIn{RequestID: "x"}); err == nil || f.nCalls() != n {
		t.Error("a launch without a mode was sent")
	}
}

func TestBadTokenIsRefusedWithoutLeaking(t *testing.T) {
	f := newFake(t, allTools...)
	c := New(f.base(), Token{Access: "wrong-secret-token"}, Auto)
	defer c.Close()
	_, err := c.Projects(context.Background())
	if err == nil {
		t.Fatal("a wrong token worked")
	}
	if strings.Contains(err.Error(), "wrong-secret-token") {
		t.Errorf("error leaks the token: %v", err)
	}
}

func TestCreateProjectChecksTheRoot(t *testing.T) {
	root := t.TempDir()
	f := newFake(t, allTools...)
	c := f.client(Auto)
	ctx := context.Background()

	f.reply["t3_project_create"] = func(a map[string]any) (any, string) {
		return map[string]any{"id": "p1", "title": a["title"], "workspaceRoot": a["workspaceRoot"]}, ""
	}
	p, err := c.CreateProject(ctx, root, "demo")
	if err != nil || p.ID != "p1" || p.Title != "demo" || p.WorkspaceRoot != root {
		t.Fatalf("project = %+v, %v", p, err)
	}
	if got := f.last().Args; !reflect.DeepEqual(keys(got), []string{"title", "workspaceRoot"}) || got["workspaceRoot"] != root {
		t.Errorf("args = %v", got)
	}

	// T3 ignored the root and used its own folder.
	f.reply["t3_project_create"] = func(a map[string]any) (any, string) {
		return map[string]any{"id": "p2", "title": a["title"], "workspaceRoot": filepath.Join(t.TempDir(), "default")}, ""
	}
	if _, err := c.CreateProject(ctx, root, "demo"); err == nil || !strings.Contains(err.Error(), "different folder") {
		t.Errorf("a project in another folder: %v", err)
	}

	n := f.nCalls()
	if _, err := c.CreateProject(ctx, "relative/dir", "demo"); err == nil || f.nCalls() != n {
		t.Error("a relative root was sent")
	}
}

func TestToolError(t *testing.T) {
	f := newFake(t, allTools...)
	f.reply["t3_thread_send"] = func(map[string]any) (any, string) { return nil, "thread is archived" }
	_, err := f.client(Auto).Send(context.Background(), "th1", "hi", "r")
	var te *ToolError
	if !errors.As(err, &te) || te.Tool != "t3_thread_send" || te.Message != "thread is archived" {
		t.Errorf("err = %v", err)
	}
}

func TestReconnectsOnceWhenTheSessionIsDropped(t *testing.T) {
	f := newFake(t, allTools...)
	f.reply["t3_thread_interrupt"] = func(map[string]any) (any, string) { return map[string]any{}, "" }
	c := f.client(Auto)
	ctx := context.Background()
	if err := c.Interrupt(ctx, "th1"); err != nil {
		t.Fatal(err)
	}
	f.set(func() { f.dropOnce = true })
	if err := c.Interrupt(ctx, "th1"); err != nil {
		t.Fatalf("after the drop: %v", err)
	}
	f.mu.Lock()
	inits := len(f.sessions)
	f.mu.Unlock()
	if inits != 2 {
		t.Errorf("sessions opened = %d, want 2", inits)
	}
	if f.nCalls() != 2 {
		t.Errorf("calls reaching the tool = %d, want 2", f.nCalls())
	}
}

func TestProjectsFollowsTheCursor(t *testing.T) {
	f := newFake(t, allTools...)
	f.reply["t3_project_list"] = func(a map[string]any) (any, string) {
		if a["cursor"] == nil {
			return map[string]any{"projects": []any{map[string]any{"id": "a"}}, "nextCursor": "c1"}, ""
		}
		return map[string]any{"projects": []any{map[string]any{"id": "b"}}}, ""
	}
	ps, err := f.client(Auto).Projects(context.Background())
	if err != nil || len(ps) != 2 || ps[0].ID != "a" || ps[1].ID != "b" {
		t.Errorf("projects = %+v, %v", ps, err)
	}
	if f.last().Args["cursor"] != "c1" {
		t.Errorf("last args = %v", f.last().Args)
	}
}

func TestThreadOps(t *testing.T) {
	f := newFake(t, allTools...)
	f.reply["t3_thread_send"] = func(map[string]any) (any, string) {
		return map[string]any{"threadId": "th1", "messageId": "m1", "runId": "r2", "status": "running", "delivery": "started"}, ""
	}
	f.reply["t3_thread_wait"] = func(map[string]any) (any, string) {
		return map[string]any{"threadId": "th1", "runId": "r2", "status": "completed", "timedOut": false}, ""
	}
	f.reply["t3_thread_read"] = func(map[string]any) (any, string) {
		return map[string]any{
			"thread": map[string]any{"threadId": "th1", "projectId": "p1", "status": "idle", "providerInstanceId": "i", "runtimeMode": "auto", "latestRunId": "r2"},
			"items":  []any{map[string]any{"position": 3, "type": "assistant_message", "text": "MOCK-OK hello", "runId": "r2", "createdBy": "assistant"}},
		}, ""
	}
	f.reply["t3_thread_transfer"] = func(map[string]any) (any, string) {
		return map[string]any{"sequence": 9, "targetThreadId": "th2"}, ""
	}
	c := f.client(Auto)
	ctx := context.Background()

	run, err := c.Send(ctx, "th1", "more", "req-2")
	if err != nil || run.MessageID != "m1" || run.RunID != "r2" || run.Delivery != "started" {
		t.Errorf("send = %+v, %v", run, err)
	}
	if got := f.last().Args; !reflect.DeepEqual(keys(got), []string{"clientRequestId", "message", "threadId"}) || got["clientRequestId"] != "req-2" {
		t.Errorf("send args = %v", got)
	}

	w, err := c.Wait(ctx, "th1", 90*time.Second)
	if err != nil || !w.Done() || w.TimedOut {
		t.Errorf("wait = %+v, %v", w, err)
	}
	if got := f.last().Args; !reflect.DeepEqual(keys(got), []string{"threadId", "timeoutMs"}) || got["timeoutMs"] != float64(90000) {
		t.Errorf("wait args = %v", got)
	}

	th, items, err := c.Read(ctx, "th1", 2)
	if err != nil || th.ProjectID != "p1" || th.LatestRunID != "r2" || len(items) != 1 || items[0].Text != "MOCK-OK hello" || items[0].Position != 3 {
		t.Errorf("read = %+v %+v, %v", th, items, err)
	}
	if got := f.last().Args; !reflect.DeepEqual(keys(got), []string{"afterPosition", "threadId", "view"}) || got["view"] != "messages" {
		t.Errorf("read args = %v", got)
	}

	id, err := c.Fork(ctx, "th1", "copy")
	if err != nil || id != "th2" {
		t.Errorf("fork = %q, %v", id, err)
	}
	if got := f.last().Args; !reflect.DeepEqual(keys(got), []string{"action", "threadId", "title"}) || got["action"] != "fork" {
		t.Errorf("fork args = %v", got)
	}

	if err := c.Interrupt(ctx, "th1"); err != nil {
		t.Error(err)
	}
	if got := f.last().Args; !reflect.DeepEqual(keys(got), []string{"threadId"}) {
		t.Errorf("interrupt args = %v", got)
	}
}

func TestTerminal(t *testing.T) {
	for s, want := range map[string]bool{"completed": true, "failed": true, "cancelled": true, "interrupted": true,
		"rolled_back": true, "running": false, "": false, "starting": false} {
		if Terminal(s) != want {
			t.Errorf("Terminal(%q) = %v", s, !want)
		}
	}
}

func TestLaunchRefusesAThreadAboveTheCeiling(t *testing.T) {
	f := newFake(t, allTools...)
	f.reply["t3_thread_launch"] = func(map[string]any) (any, string) {
		return map[string]any{"threadId": "th1", "runId": "r1", "status": "running"}, ""
	}
	for mode, wantErr := range map[string]bool{"full-access": true, "auto": true, "bogus": true, "": true, "approval-required": false} {
		f.reply["t3_thread_read"] = func(map[string]any) (any, string) {
			return map[string]any{"thread": map[string]any{"threadId": "th1", "runtimeMode": mode}}, ""
		}
		run, err := f.client(ApprovalRequired).Launch(context.Background(), LaunchIn{Mode: ApprovalRequired, RequestID: "x"})
		if (err != nil) != wantErr {
			t.Errorf("thread in %q: err = %v", mode, err)
		}
		if wantErr && run.ThreadID != "th1" {
			t.Errorf("thread in %q: the caller is not told which thread to stop", mode)
		}
	}
}

func TestRetryReusesTheRequestID(t *testing.T) {
	for name, do := range map[string]func(c *Client) error{
		"launch": func(c *Client) error {
			_, err := c.Launch(context.Background(), LaunchIn{Mode: Auto})
			return err
		},
		"send": func(c *Client) error {
			_, err := c.Send(context.Background(), "th1", "hi", "")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, allTools...)
			f.reply["t3_thread_launch"] = func(map[string]any) (any, string) {
				return map[string]any{"threadId": "th1", "runId": "r1", "status": "running"}, ""
			}
			f.reply["t3_thread_read"] = func(map[string]any) (any, string) {
				return map[string]any{"thread": map[string]any{"threadId": "th1", "runtimeMode": "auto"}}, ""
			}
			c := f.client(Auto)
			if _, err := c.Projects(context.Background()); err != nil { // open the session first
				t.Fatal(err)
			}
			f.set(func() { f.dropOnce = true }) // the first attempt is lost after it reached the server
			if err := do(c); err != nil {
				t.Fatal(err)
			}
			var sent, lost string
			f.set(func() { lost = f.droppedID })
			for _, cl := range f.calls {
				if id, _ := cl.Args["clientRequestId"].(string); id != "" {
					sent = id
				}
			}
			if lost == "" || sent != lost {
				t.Errorf("first attempt sent id %q, the retry %q", lost, sent)
			}
		})
	}
}

func TestBearerNeverLeavesTheBase(t *testing.T) {
	var hits int
	var auth string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		auth = r.Header.Get("Authorization")
	}))
	defer other.Close()
	f := newFake(t, allTools...)
	f.set(func() { f.redirectMCP = other.URL + "/mcp" })
	if _, err := f.client(Auto).Projects(context.Background()); err == nil {
		t.Error("a redirected server answered")
	}
	if hits != 0 || auth != "" {
		t.Errorf("the other server got %d requests, Authorization %q", hits, auth)
	}
}

func TestTransportRefusesOtherHosts(t *testing.T) {
	hits := 0
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer other.Close()
	f := newFake(t, allTools...)
	c := f.client(Auto)
	req, _ := http.NewRequest("POST", other.URL+"/mcp", nil)
	resp, err := c.hc.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Error("a request to another server went out")
	}
	if hits != 0 {
		t.Error("the other server was reached")
	}
	if err != nil && strings.Contains(err.Error(), fakeToken) {
		t.Error("error leaks the token")
	}
}

func TestClientPrintHidesTheToken(t *testing.T) {
	c := New("http://127.0.0.1:1", Token{Access: fakeToken}, Auto)
	for _, s := range []string{fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), fmt.Sprint(c),
		fmt.Sprintf("%+v", struct{ C *Client }{c})} {
		if strings.Contains(s, fakeToken) {
			t.Errorf("print shows the token: %s", s)
		}
	}
}

func TestCapBody(t *testing.T) {
	b := capBody(io.NopCloser(strings.NewReader("0123456789")), 4)
	got, _ := io.ReadAll(b)
	if string(got) != "0123" {
		t.Errorf("read %q", got)
	}
}
