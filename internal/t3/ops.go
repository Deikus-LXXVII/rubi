package t3

// Typed requests for the T3 tools Rubi uses. Each one sends only the keys the tool declares: T3 ignores an
// unknown key without a word (a wrong "path" made a project in T3's default folder), so nothing is passed
// through as a loose map.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// The T3 tools the bridge calls. The contract snapshot (t3/contract/tools.json) holds the input schemas of
// exactly these.
const (
	toolProjectList     = "t3_project_list"
	toolProjectCreate   = "t3_project_create"
	toolThreadLaunch    = "t3_thread_launch"
	toolThreadSend      = "t3_thread_send"
	toolThreadWait      = "t3_thread_wait"
	toolThreadRead      = "t3_thread_read"
	toolThreadInterrupt = "t3_thread_interrupt"
	toolThreadTransfer  = "t3_thread_transfer"
	toolThreadUpdate    = "t3_thread_update"
)

// UsedTools lists the tool names above.
var UsedTools = []string{toolProjectList, toolProjectCreate, toolThreadLaunch, toolThreadSend, toolThreadWait,
	toolThreadRead, toolThreadInterrupt, toolThreadTransfer, toolThreadUpdate}

// Project is a T3 project.
type Project struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	WorkspaceRoot string `json:"workspaceRoot"`
}

// Run is what starting or continuing a thread gives.
type Run struct {
	ThreadID  string `json:"threadId"`
	Link      string `json:"link,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
	MessageID string `json:"messageId,omitempty"`
	RunID     string `json:"runId"`
	Status    string `json:"status"`
	Delivery  string `json:"delivery,omitempty"`
}

// WaitResult is the state of a run after a wait.
type WaitResult struct {
	ThreadID string `json:"threadId"`
	RunID    string `json:"runId"`
	Status   string `json:"status"`
	TimedOut bool   `json:"timedOut"`
}

// Done reports whether the run has ended.
func (w WaitResult) Done() bool { return Terminal(w.Status) }

// Terminal reports whether a run state is final.
func Terminal(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "interrupted", "rolled_back":
		return true
	}
	return false
}

// Thread is the header of a thread read.
type Thread struct {
	ThreadID           string `json:"threadId"`
	ProjectID          string `json:"projectId"`
	Status             string `json:"status"`
	ProviderInstanceID string `json:"providerInstanceId"`
	RuntimeMode        string `json:"runtimeMode"`
	LatestRunID        string `json:"latestRunId"`
	ActiveRunID        string `json:"activeRunId"`
}

func (t Thread) mode() Mode { return Mode(t.RuntimeMode) }

// Item is one message of a thread.
type Item struct {
	Position  int    `json:"position"`
	Type      string `json:"type"`
	Text      string `json:"text"`
	RunID     string `json:"runId"`
	CreatedBy string `json:"createdBy"`
}

// LaunchIn is a new thread. Mode is required; it is clamped to the client's ceiling.
type LaunchIn struct {
	ProjectID string
	Title     string
	Instance  string
	Model     string
	Mode      Mode
	// Plan starts the thread in T3's plan interaction mode (a claude "plan" session); the runtime mode still applies.
	Plan      bool
	Message   string
	RequestID string
}

// Projects lists all projects.
func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	type in struct {
		Cursor string `json:"cursor,omitempty"`
	}
	var all []Project
	cursor := ""
	for page := 0; page < 1000; page++ {
		var out struct {
			Projects   []Project `json:"projects"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := c.call(ctx, toolProjectList, in{cursor}, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Projects...)
		if out.NextCursor == "" || out.NextCursor == cursor {
			return all, nil
		}
		cursor = out.NextCursor
	}
	return nil, errors.New("t3: project list does not end")
}

// CreateProject makes a project rooted at root. T3 answering with another root is an error.
func (c *Client) CreateProject(ctx context.Context, root, title string) (Project, error) {
	if !filepath.IsAbs(root) {
		return Project{}, errors.New("t3: project root must be an absolute path")
	}
	type in struct {
		Title         string `json:"title"`
		WorkspaceRoot string `json:"workspaceRoot"`
	}
	var p Project
	if err := c.call(ctx, toolProjectCreate, in{title, root}, &p); err != nil {
		return Project{}, err
	}
	if !sameDir(p.WorkspaceRoot, root) {
		return Project{}, errors.New("t3: project was created in a different folder than asked: " + p.WorkspaceRoot)
	}
	return p, nil
}

// sameDir compares two folders, looking through links when both exist.
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	return ea == nil && eb == nil && ra == rb
}

// Launch starts a thread and its first run. An empty RequestID is made up, once, before the first attempt.
func (c *Client) Launch(ctx context.Context, l LaunchIn) (Run, error) {
	if l.Mode == "" {
		return Run{}, errors.New("t3: launch needs a mode")
	}
	type sel struct {
		InstanceID string `json:"instanceId"`
		Model      string `json:"model"`
	}
	type in struct {
		ProjectID      string `json:"projectId"`
		Title          string `json:"title"`
		ModelSelection sel    `json:"modelSelection"`
		RuntimeMode    Mode   `json:"runtimeMode"`
		Interaction    string `json:"interactionMode,omitempty"`
		Message        string `json:"message"`
		ClientRequest  string `json:"clientRequestId"`
	}
	interaction := ""
	if l.Plan {
		interaction = "plan"
	}
	if l.RequestID == "" {
		// Chosen before the first attempt, so a retry after a dropped connection cannot start a second thread.
		id, err := newRequestID()
		if err != nil {
			return Run{}, err
		}
		l.RequestID = id
	}
	var r Run
	err := c.call(ctx, toolThreadLaunch, in{l.ProjectID, l.Title, sel{l.Instance, l.Model}, Clamp(l.Mode, c.ceiling), interaction, l.Message, l.RequestID}, &r)
	if err != nil {
		return r, err
	}
	// Defense in depth: T3 must have taken the clamped mode.
	th, _, err := c.Read(ctx, r.ThreadID, 0)
	if err != nil {
		return r, err
	}
	if !th.mode().known() || th.mode().rank() > c.ceiling.rank() {
		return r, fmt.Errorf("t3: thread %s runs in mode %q, above the ceiling %s", r.ThreadID, th.RuntimeMode, c.ceiling)
	}
	return r, nil
}

// newRequestID makes an id for an idempotent request.
func newRequestID() (string, error) {
	id, err := randomString()
	if err != nil {
		return "", err
	}
	return "rubi-" + id, nil
}

// Send adds a message to a thread. The same requestID sends it once; an empty one is made up.
func (c *Client) Send(ctx context.Context, threadID, message, requestID string) (Run, error) {
	return c.SendWith(ctx, threadID, message, requestID, "")
}

// SendWith is Send with T3's delivery mode for a thread that is running: "" (T3 decides: auto), "queue", "steer" or
// "restart".
func (c *Client) SendWith(ctx context.Context, threadID, message, requestID, mode string) (Run, error) {
	if requestID == "" {
		id, err := newRequestID()
		if err != nil {
			return Run{}, err
		}
		requestID = id
	}
	type in struct {
		ThreadID      string `json:"threadId"`
		Message       string `json:"message"`
		Mode          string `json:"mode,omitempty"`
		ClientRequest string `json:"clientRequestId"`
	}
	var r Run
	err := c.call(ctx, toolThreadSend, in{threadID, message, mode, requestID}, &r)
	return r, err
}

// Wait blocks until the thread's run ends or the timeout passes (then TimedOut is set).
func (c *Client) Wait(ctx context.Context, threadID string, timeout time.Duration) (WaitResult, error) {
	return c.WaitRun(ctx, threadID, "", timeout)
}

// WaitRun is Wait for one run of the thread (an empty runID waits for the thread's latest).
func (c *Client) WaitRun(ctx context.Context, threadID, runID string, timeout time.Duration) (WaitResult, error) {
	type in struct {
		ThreadID  string `json:"threadId"`
		RunID     string `json:"runId,omitempty"`
		TimeoutMs int64  `json:"timeoutMs"`
	}
	var w WaitResult
	err := c.call(ctx, toolThreadWait, in{threadID, runID, timeout.Milliseconds()}, &w)
	return w, err
}

// Read returns the thread header and the messages after the given position.
func (c *Client) Read(ctx context.Context, threadID string, after int) (Thread, []Item, error) {
	type in struct {
		ThreadID      string `json:"threadId"`
		View          string `json:"view"`
		AfterPosition int    `json:"afterPosition,omitempty"`
	}
	var out struct {
		Thread Thread `json:"thread"`
		Items  []Item `json:"items"`
	}
	err := c.call(ctx, toolThreadRead, in{threadID, "messages", after}, &out)
	return out.Thread, out.Items, err
}

// Interrupt stops the thread's active run.
func (c *Client) Interrupt(ctx context.Context, threadID string) error {
	type in struct {
		ThreadID string `json:"threadId"`
	}
	return c.call(ctx, toolThreadInterrupt, in{threadID}, nil)
}

// Fork copies a thread, history included, and returns the new thread's id.
func (c *Client) Fork(ctx context.Context, threadID, title string) (string, error) {
	type in struct {
		Action   string `json:"action"`
		ThreadID string `json:"threadId"`
		Title    string `json:"title"`
	}
	var out struct {
		Target string `json:"targetThreadId"`
	}
	if err := c.call(ctx, toolThreadTransfer, in{"fork", threadID, title}, &out); err != nil {
		return "", err
	}
	if out.Target == "" {
		return "", errors.New("t3: fork gave no thread")
	}
	return out.Target, nil
}

// Rename gives a thread a new title.
func (c *Client) Rename(ctx context.Context, threadID, title string) error {
	type in struct {
		Action   string `json:"action"`
		ThreadID string `json:"threadId"`
		Title    string `json:"title"`
	}
	return c.call(ctx, toolThreadUpdate, in{"rename", threadID, title}, nil)
}
