// Package t3test is a stand-in for a T3 server's MCP endpoint, for tests of code that uses internal/t3's client
// (Rubi Home's t3-agent ops). It keeps projects, threads, runs and messages in memory, records every tool call with
// its arguments and headers, and lets a test finish a run or fail a tool.
package t3test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Token is the bearer token the stand-in accepts.
const Token = "tok-fake-t3-not-a-secret"

// Call is one tool call the stand-in saw.
type Call struct {
	Tool   string
	Args   map[string]any
	Header http.Header
}

type item struct {
	Position  int    `json:"position"`
	Type      string `json:"type"`
	Text      string `json:"text"`
	RunID     string `json:"runId"`
	CreatedBy string `json:"createdBy"`
}

type run struct {
	id     string
	status string
}

type thread struct {
	id, project, instance, title, runtimeMode, interaction string
	items                                                  []item
	runs                                                   []*run
	requests                                               map[string]string // clientRequestId -> run id
}

// Project is a project the stand-in holds.
type Project struct{ ID, Title, Root string }

// Fake is the stand-in.
type Fake struct {
	t   *testing.T
	Srv *httptest.Server

	mu       sync.Mutex
	calls    []Call
	projects []Project
	threads  map[string]*thread
	seq      int
	once     sync.Once
	done     chan struct{} // closed at cleanup, so a wait in progress ends
	changed  chan struct{} // closed and replaced when a run changes
	fail     map[string]string
	// WrongRoot, when set, makes project_create answer with this folder instead of the one asked for.
	WrongRoot string
	// SendDelivery is the "delivery" a send reports.
	SendDelivery string
	// ThreadIDPrefix exercises native opaque IDs rather than runner filename IDs.
	ThreadIDPrefix string
}

var tools = []string{"t3_project_list", "t3_project_create", "t3_thread_launch", "t3_thread_send", "t3_thread_wait", "t3_thread_read",
	"t3_thread_interrupt", "t3_thread_transfer", "t3_thread_update"}

// New starts the stand-in; it stops with the test.
func New(t *testing.T) *Fake {
	f := &Fake{t: t, threads: map[string]*thread{}, changed: make(chan struct{}), fail: map[string]string{}, done: make(chan struct{})}
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-t3", Version: "0"}, nil)
	for _, name := range tools {
		s.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var args map[string]any
				_ = json.Unmarshal(req.Params.Arguments, &args)
				f.mu.Lock()
				f.calls = append(f.calls, Call{req.Params.Name, args, req.Extra.Header.Clone()})
				msg := f.fail[req.Params.Name]
				f.mu.Unlock()
				if msg != "" {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil
				}
				out, errMsg := f.tool(ctx, req.Params.Name, args)
				if errMsg != "" {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: errMsg}}}, nil
				}
				return &mcp.CallToolResult{StructuredContent: out, Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
			})
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	f.Srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "Bearer "+Token {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Srv.Close)
	t.Cleanup(func() { f.Release(); f.Srv.CloseClientConnections() }) // runs first: a wait in progress ends
	return f
}

// Release ends the waits in progress (a test calls it before closing the client it used, whose close would
// otherwise wait for them).
func (f *Fake) Release() { f.once.Do(func() { close(f.done) }) }

// URL is the server's base address.
func (f *Fake) URL() string { return f.Srv.URL }

// Fail makes a tool answer with an error (an empty message clears it).
func (f *Fake) Fail(tool, msg string) {
	f.mu.Lock()
	f.fail[tool] = msg
	f.mu.Unlock()
}

// Calls returns the calls of one tool (all of them when tool is empty).
func (f *Fake) Calls(tool string) []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Call
	for _, c := range f.calls {
		if tool == "" || c.Tool == tool {
			out = append(out, c)
		}
	}
	return out
}

// AddProject puts a project in.
func (f *Fake) AddProject(id, title, root string) {
	f.mu.Lock()
	f.projects = append(f.projects, Project{id, title, root})
	f.mu.Unlock()
}

// Projects lists the projects now.
func (f *Fake) Projects() []Project {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Project(nil), f.projects...)
}

func (f *Fake) next(prefix string) string {
	if prefix == "thr" && f.ThreadIDPrefix != "" {
		prefix = f.ThreadIDPrefix
	}
	f.seq++
	return fmt.Sprintf("%s-%d", prefix, f.seq)
}

func (f *Fake) notifyLocked() {
	close(f.changed)
	f.changed = make(chan struct{})
}

// Complete ends a thread's latest run with the status, adding an assistant message first when text is not empty.
func (f *Fake) Complete(threadID, status, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	th := f.threads[threadID]
	if th == nil || len(th.runs) == 0 {
		f.t.Errorf("t3test: no run to complete in %q", threadID)
		return
	}
	r := th.runs[len(th.runs)-1]
	if text != "" {
		th.items = append(th.items, item{len(th.items) + 1, "assistant_message", text, r.id, "assistant"})
	}
	r.status = status
	f.notifyLocked()
}

// CompleteRun ends one run of a thread (not necessarily the latest), like Complete.
func (f *Fake) CompleteRun(threadID, runID, status, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	th := f.threads[threadID]
	for _, r := range th.runs {
		if r.id == runID {
			if text != "" {
				th.items = append(th.items, item{len(th.items) + 1, "assistant_message", text, r.id, "assistant"})
			}
			r.status = status
			f.notifyLocked()
			return
		}
	}
	f.t.Errorf("t3test: no run %q in %q", runID, threadID)
}

// Runs lists a thread's run ids, oldest first.
func (f *Fake) Runs(threadID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.threads[threadID].runs {
		out = append(out, r.id)
	}
	return out
}

// AddItem puts a message in a thread (as if typed in T3's own app).
func (f *Fake) AddItem(threadID, typ, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	th := f.threads[threadID]
	th.items = append(th.items, item{len(th.items) + 1, typ, text, "", "user"})
}

// Mode is the runtime mode a thread was made with, and its interaction mode.
func (f *Fake) Mode(threadID string) (runtime, interaction string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	th := f.threads[threadID]
	if th == nil {
		return "", ""
	}
	return th.runtimeMode, th.interaction
}

// Title is a thread's title.
func (f *Fake) Title(threadID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if th := f.threads[threadID]; th != nil {
		return th.title
	}
	return ""
}

func str(a map[string]any, k string) string { s, _ := a[k].(string); return s }

func (f *Fake) tool(ctx context.Context, name string, a map[string]any) (any, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch name {
	case "t3_project_list":
		ps := []map[string]any{}
		for _, p := range f.projects {
			ps = append(ps, map[string]any{"id": p.ID, "title": p.Title, "workspaceRoot": p.Root})
		}
		return map[string]any{"projects": ps}, ""
	case "t3_project_create":
		root := str(a, "workspaceRoot")
		p := Project{f.next("proj"), str(a, "title"), root}
		f.projects = append(f.projects, p)
		if f.WrongRoot != "" {
			root = f.WrongRoot
		}
		return map[string]any{"id": p.ID, "title": p.Title, "workspaceRoot": root}, ""
	case "t3_thread_launch":
		th := &thread{id: f.next("thr"), project: str(a, "projectId"), title: str(a, "title"), runtimeMode: str(a, "runtimeMode"),
			interaction: str(a, "interactionMode"), requests: map[string]string{}}
		if sel, ok := a["modelSelection"].(map[string]any); ok {
			th.instance = str(sel, "instanceId")
		}
		f.threads[th.id] = th
		r := f.startRun(th, str(a, "message"), str(a, "clientRequestId"))
		return map[string]any{"threadId": th.id, "projectId": th.project, "runId": r.id, "status": r.status}, ""
	case "t3_thread_send":
		th := f.threads[str(a, "threadId")]
		if th == nil {
			return nil, "no such thread"
		}
		r := f.startRun(th, str(a, "message"), str(a, "clientRequestId"))
		d := f.SendDelivery
		if d == "" {
			d = "started"
		}
		return map[string]any{"threadId": th.id, "runId": r.id, "status": r.status, "delivery": d}, ""
	case "t3_thread_wait":
		th := f.threads[str(a, "threadId")]
		if th == nil || len(th.runs) == 0 {
			return nil, "no such thread"
		}
		runID := str(a, "runId")
		timeout := time.Duration(1000) * time.Millisecond
		if ms, ok := a["timeoutMs"].(float64); ok {
			timeout = time.Duration(ms) * time.Millisecond
		}
		deadline := time.Now().Add(timeout)
		for {
			r := th.runs[len(th.runs)-1]
			if runID != "" {
				r = nil
				for _, x := range th.runs {
					if x.id == runID {
						r = x
					}
				}
				if r == nil {
					return nil, "no such run"
				}
			}
			if r.status != "running" || time.Now().After(deadline) {
				return map[string]any{"threadId": th.id, "runId": r.id, "status": r.status, "timedOut": r.status == "running"}, ""
			}
			ch := f.changed
			f.mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				f.mu.Lock()
				return nil, "cancelled"
			case <-f.done:
				f.mu.Lock()
				return nil, "cancelled"
			case <-time.After(time.Until(deadline)):
			}
			f.mu.Lock()
		}
	case "t3_thread_read":
		th := f.threads[str(a, "threadId")]
		if th == nil {
			return nil, "no such thread"
		}
		after, _ := a["afterPosition"].(float64)
		items := []item{}
		for _, it := range th.items {
			if float64(it.Position) > after {
				items = append(items, it)
			}
		}
		latest := ""
		if len(th.runs) > 0 {
			latest = th.runs[len(th.runs)-1].id
		}
		return map[string]any{"thread": map[string]any{"threadId": th.id, "projectId": th.project, "providerInstanceId": th.instance,
			"runtimeMode": th.runtimeMode, "latestRunId": latest}, "items": items}, ""
	case "t3_thread_interrupt":
		th := f.threads[str(a, "threadId")]
		if th == nil {
			return nil, "no such thread"
		}
		if len(th.runs) > 0 && th.runs[len(th.runs)-1].status == "running" {
			th.runs[len(th.runs)-1].status = "interrupted"
			f.notifyLocked()
		}
		return map[string]any{}, ""
	case "t3_thread_transfer":
		src := f.threads[str(a, "threadId")]
		if src == nil || str(a, "action") != "fork" {
			return nil, "cannot transfer"
		}
		nt := &thread{id: f.next("thr"), project: src.project, instance: src.instance, title: str(a, "title"), runtimeMode: src.runtimeMode,
			interaction: src.interaction, requests: map[string]string{}, items: append([]item(nil), src.items...)}
		f.threads[nt.id] = nt
		return map[string]any{"targetThreadId": nt.id}, ""
	case "t3_thread_update":
		th := f.threads[str(a, "threadId")]
		if th == nil || str(a, "action") != "rename" {
			return nil, "cannot update"
		}
		th.title = str(a, "title")
		return map[string]any{}, ""
	}
	return nil, "unknown tool"
}

// startRun adds the user's message and a running run; the same clientRequestId gives the run it made first.
func (f *Fake) startRun(th *thread, message, request string) *run {
	if request != "" {
		if id, ok := th.requests[request]; ok {
			for _, r := range th.runs {
				if r.id == id {
					return r
				}
			}
		}
	}
	r := &run{id: f.next("run"), status: "running"}
	th.runs = append(th.runs, r)
	if message != "" {
		th.items = append(th.items, item{len(th.items) + 1, "user_message", message, r.id, "agent"})
	}
	if request != "" {
		th.requests[request] = r.id
	}
	return r
}
