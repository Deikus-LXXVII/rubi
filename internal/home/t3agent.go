package home

// The t3-agent.* ops: the claude-agent.* contract (same names, arguments and result shapes) backed by T3 threads
// instead of the Agent SDK runner (docs/plans/t3-integration.md section 13). A session id is a T3 thread id; a role
// is a T3 provider instance `claude-<role>` whose claude runs in the role's VM; the folder is the role's pinned
// project folder. Tool asks arrive through the VM's hook gate into the same approval queue claude-agent.next serves,
// so next and decide are the manager's own. role.*, ssh.*, limits and usage_mode are the existing implementations.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"github.com/Deikus-LXXVII/rubi/internal/agenttask"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/t3vm"
	"github.com/Deikus-LXXVII/rubi/internal/t3"
)

var modelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]-]{0,79}$`)

func evalLink(p string) (string, error) { return filepath.EvalSymlinks(p) }

// How long one wait on a thread's run lasts before it is asked again.
const t3WaitSlice = 25 * time.Second

// maxResultRunes bounds the text of a finished turn that goes into an event.
const maxResultRunes = 8000

// t3Ready is the client, or a plain error that names the setup step to run.
func (t *T3Home) ready() (*t3.Client, error) {
	if cl := t.client(); cl != nil {
		return cl, nil
	}
	if _, ok := t.installedNow(); !ok {
		return nil, errors.New("T3 isn't installed on this computer yet: run the install step of the T3 setup (t3-setup.run, step install)")
	}
	if !t.srvAlive() {
		return nil, errors.New("T3 isn't running: run the start step of the T3 setup (t3-setup.run, step start)")
	}
	return nil, errors.New("T3 isn't paired with Rubi Home: run the pair step of the T3 setup (t3-setup.run, step pair)")
}

func newMsgID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic("no randomness: " + err.Error())
	}
	return "rubi-" + hex.EncodeToString(b)
}

// handleAgent serves one t3-agent.* op.
func (t *T3Home) handleAgent(ctx context.Context, op string, args json.RawMessage) (any, error) {
	if strings.HasPrefix(op, "t3-agent.task.") {
		return t.handleTask(ctx, op, args)
	}
	name := strings.TrimPrefix(op, "t3-agent.")
	switch {
	case strings.HasPrefix(name, "role."):
		res, err := t.h.Agent.Handle(ctx, "claude-agent."+name, args)
		if err == nil && name == "role.put" {
			// The role's instance in T3 follows the role.
			if sr, serr := t.h.SyncT3Instances(); serr != nil {
				log.Printf("T3 role instances: %v", serr)
			} else if sr.Changed() {
				log.Printf("T3 role instances: added %v, updated %v, removed %v", sr.Added, sr.Updated, sr.Removed)
			}
		}
		return res, err
	case strings.HasPrefix(name, "ssh."), name == "next", name == "decide", name == "usage_mode", name == "limits":
		return t.h.Agent.Handle(ctx, "claude-agent."+name, args)
	case name == "status":
		return t.status(ctx)
	}
	var in struct {
		Task         bool              `json:"task"`
		Publish      *agenttask.Target `json:"publish"`
		AutoFinish   bool              `json:"auto_finish"`
		PublishProof string            `json:"publish_proof"`
		Project      string            `json:"project"`
		Role         string            `json:"role"`
		ID           string            `json:"id"`
		Limit        int               `json:"limit"`
		Prompt       string            `json:"prompt"`
		Text         string            `json:"text"`
		Name         string            `json:"name"`
		From         string            `json:"from"`
		Mode         string            `json:"mode"`
		Steer        bool              `json:"steer"`
		Bot          string            `json:"bot"`
		Model        string            `json:"model"`
		Provider     string            `json:"provider"`
		MessageID    string            `json:"message_id"`
	}
	if len(args) > 0 && string(args) != "null" && json.Unmarshal(args, &in) != nil {
		return nil, errors.New("bad arguments")
	}
	short := func(s string, n int) bool { return len([]rune(s)) <= n && !strings.ContainsAny(s, "\x00\r") }
	if !short(in.Name, 64) || !short(in.From, 64) || len(in.Prompt) > agent.MaxText || len(in.Text) > agent.MaxText {
		return nil, errors.New("a value is too long")
	}
	switch name {
	case "projects":
		return t.projects(), nil
	case "sessions":
		return t.sessions(in.Project, in.Role), nil
	case "messages":
		if !validT3ThreadID(in.ID) {
			return nil, errors.New("bad session id")
		}
		return t.messages(ctx, in.Project, in.Role, in.ID, in.Limit)
	case "start":
		return t.start(ctx, startIn{project: in.Project, role: in.Role, prompt: in.Prompt, name: in.Name, from: in.From, mode: in.Mode, bot: in.Bot, model: in.Model, messageID: in.MessageID, provider: in.Provider, task: in.Task, autoFinish: in.AutoFinish, publish: in.Publish, proof: in.PublishProof})
	case "send":
		if !validT3ThreadID(in.ID) {
			return nil, errors.New("bad session id")
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, errors.New("the text is empty")
		}
		if !agent.OkBot(in.Bot) {
			return nil, errors.New("bad bot id")
		}
		return t.send(ctx, in.Project, in.Role, in.ID, in.Text, in.From, in.Bot, in.MessageID, in.Steer)
	case "rename":
		if !validT3ThreadID(in.ID) || strings.TrimSpace(in.Name) == "" {
			return nil, errors.New("a session id and a name are needed")
		}
		return t.rename(ctx, in.Project, in.Role, in.ID, in.Name)
	case "fork":
		if !validT3ThreadID(in.ID) {
			return nil, errors.New("bad session id")
		}
		return t.fork(ctx, in.Project, in.Role, in.ID, in.Name)
	case "interrupt":
		if !validT3ThreadID(in.ID) {
			return nil, errors.New("bad session id")
		}
		return t.interrupt(ctx, in.Project, in.Role, in.ID)
	case "close":
		if !validT3ThreadID(in.ID) {
			return nil, errors.New("bad session id")
		}
		return t.closeThread(ctx, in.Project, in.Role, in.ID)
	case "mode":
		if in.Mode == "" || !validT3ThreadID(in.ID) {
			return nil, errors.New("a session id and a valid mode are needed")
		}
		return t.setMode(ctx, in.Project, in.Role, in.ID, in.Mode)
	}
	return nil, fmt.Errorf("Rubi Home doesn't do %q", op)
}

// own finds a thread Home started, in the role the call names (a missing role is the thread's own).
func (t *T3Home) own(project, role, id string) (t3Thread, error) {
	th, ok := t.threads.get(id)
	if !ok {
		return th, fmt.Errorf("session %s isn't one of the T3 sessions Rubi Home started", clipStr(id, 40))
	}
	if (project != "" && th.Project != project) || (role != "" && th.Role != role) {
		return th, fmt.Errorf("session %s doesn't belong to that project and role", clipStr(id, 40))
	}
	return th, nil
}

func (t *T3Home) ownOpen(project, role, id string) (t3Thread, error) {
	th, err := t.own(project, role, id)
	if err == nil && th.Closed {
		return th, fmt.Errorf("session %s is closed", clipStr(id, 40))
	}
	return th, err
}

// ---- status, projects, sessions ----

// status is claude-agent.status with the T3 threads that run counted in (a probe is never started: it would need a
// runner VM, and T3 sessions have none of that kind).
func (t *T3Home) status(ctx context.Context) (any, error) {
	res, err := t.h.Agent.Handle(ctx, "claude-agent.status", json.RawMessage(`{}`))
	if err != nil {
		return nil, err
	}
	m, ok := res.(map[string]any)
	if !ok {
		return res, nil
	}
	slots, _ := m["slots"].([]map[string]any)
	before := len(slots)
	for _, th := range t.threads.open("", "") {
		if !th.Running {
			continue
		}
		slots = append(slots, map[string]any{"project": th.Project, "role": th.Role, "role_name": t.roleName(th.Project, th.Role), "session": th.ID,
			"busy": true, "background": 0, "subagents": 0, "bot": th.Bot})
	}
	m["slots"] = slots
	if before == 0 && len(slots) > 0 {
		m["slot"] = slots[0]
	}
	n, _ := m["sessions_running"].(int)
	m["sessions_running"] = n + len(slots) - before
	_, installed := t.installedNow()
	m["t3"] = map[string]any{"installed": installed, "running": t.srvAlive(), "paired": t.client() != nil}
	return m, nil
}

func (t *T3Home) roleName(project, role string) string {
	for _, r := range t.h.Agent.T3Projects() {
		if r.Role == role {
			return r.RoleName
		}
	}
	return role
}

// projects is claude-agent.projects for the isolated roles (a T3 session always runs in a role).
func (t *T3Home) projects() any {
	type row struct {
		ID       string      `json:"id"`
		Dir      string      `json:"dir"`
		Role     string      `json:"role"`
		RoleName string      `json:"role_name"`
		VMState  agent.State `json:"vm_state"`
		SlotBusy bool        `json:"slot_busy"`
		Queue    int         `json:"queue"`
		Running  int         `json:"sessions_running"`
	}
	per := t.h.Agent.T3LimitsFor("").PerRole
	rows := []row{}
	for _, r := range t.h.Agent.T3Projects() {
		x := row{ID: r.Project, Dir: r.Dir, Role: r.Role, RoleName: r.RoleName, VMState: agent.StateStopped}
		x.Running = t.threads.counts(r.Project, r.Role, "").role
		if x.Running > 0 {
			x.VMState = agent.StateRunning
		}
		x.SlotBusy = per > 0 && x.Running >= per
		rows = append(rows, x)
	}
	return map[string]any{"projects": rows}
}

func (t *T3Home) sessions(project, role string) any {
	type row struct {
		ID           string    `json:"id"`
		Dir          string    `json:"dir"` // the trusted session workspace, never an ambient cwd
		Task         string    `json:"task,omitempty"`
		Branch       string    `json:"branch,omitempty"`
		Name         string    `json:"name"`
		Created      time.Time `json:"created"`
		LastActivity time.Time `json:"last_activity"`
		Mode         string    `json:"mode"`
		Busy         bool      `json:"busy"`
	}
	rows := []row{}
	dirs := map[string]string{}
	for _, r := range t.h.Agent.T3Projects() {
		dirs[r.Project] = r.Dir
	}
	for _, th := range t.threads.open(project, role) {
		dir := dirs[th.Project]
		if th.Task != "" {
			w, err := agenttask.Resolve(t.h.Dir, th.Task, th.Project, th.Role, roles.DirHash(dir))
			dir = ""
			if err == nil {
				dir = w.Dir
			}
		}
		rows = append(rows, row{ID: th.ID, Dir: dir, Task: th.Task, Branch: th.Branch, Name: th.Name, Created: th.Created.UTC(), LastActivity: th.LastActivity.UTC(), Mode: th.Mode, Busy: th.Running})
	}
	return map[string]any{"sessions": rows}
}

// readAll reads a thread's items after a position, page after page.
func readAll(ctx context.Context, cl *t3.Client, id string, after int) (t3.Thread, []t3.Item, error) {
	var all []t3.Item
	var head t3.Thread
	for range 200 {
		th, items, err := cl.Read(ctx, id, after)
		if err != nil {
			return th, all, err
		}
		head = th
		next := after
		for _, it := range items {
			if it.Position > next {
				next = it.Position
			}
		}
		if len(items) == 0 || next == after {
			return head, all, nil
		}
		all = append(all, items...)
		after = next
		if len(all) >= 5000 {
			break
		}
	}
	return head, all, nil
}

// messages is claude-agent.messages: the thread's messages in the SDK's shape (type, message{role, content}), the last
// `limit` of them (at most 500; 0 is 500).
func (t *T3Home) messages(ctx context.Context, project, role, id string, limit int) (any, error) {
	th, err := t.own(project, role, id)
	if err != nil {
		return nil, err
	}
	cl, err := t.readyForThread(ctx, th)
	if err != nil {
		return nil, err
	}
	_, items, err := readAll(ctx, cl, id, 0)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, it := range items {
		uuid := fmt.Sprintf("t3-%s-%d", id, it.Position)
		switch it.Type {
		case "user_message":
			out = append(out, map[string]any{"type": "user", "uuid": uuid, "session_id": id,
				"message": map[string]any{"role": "user", "content": it.Text}})
		case "assistant_message":
			out = append(out, map[string]any{"type": "assistant", "uuid": uuid, "session_id": id,
				"message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": it.Text}}}})
		}
	}
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return map[string]any{"messages": out}, nil
}

// ---- start, send ----

type startIn struct {
	project, role, prompt, name, from, mode, bot, model, messageID, provider string
	task                                                                     bool
	autoFinish                                                               bool
	publish                                                                  *agenttask.Target
	proof                                                                    string
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

func toOf(from, bot string) string {
	if f := strings.TrimSpace(from); f != "" {
		return f
	}
	if b := strings.TrimSpace(bot); b != "" {
		return b
	}
	return "local:user"
}

func labelled(from, text string) string {
	if from != "" {
		return "[message from " + from + "]\n" + text
	}
	return text
}

func (t *T3Home) checkCaps(project, role, bot string) error {
	l := t.h.Agent.T3LimitsFor(bot)
	c := t.threads.counts(project, role, bot)
	name := t.roleName(project, role)
	switch {
	case l.PerRole > 0 && c.role >= l.PerRole:
		return fmt.Errorf("role %s already runs %d session(s), its limit; wait for one to finish (or raise the limit)", clipStr(name, 40), c.role)
	case l.PerProject > 0 && c.project >= l.PerProject:
		return fmt.Errorf("project %s already runs %d session(s) in all its roles, its limit", clipStr(project, 40), c.project)
	case l.PerBot > 0 && bot != "" && c.bot >= l.PerBot:
		return fmt.Errorf("this Bot already runs %d session(s), the limit of its usage mode", c.bot)
	case l.Total > 0 && c.total >= l.Total:
		return fmt.Errorf("%d sessions already run on this computer, the limit", c.total)
	}
	return nil
}

// clampMode maps a claude-agent mode to the T3 runtime mode the thread gets: never above the role's ceiling or the
// bridge's.
func (t *T3Home) clampMode(mode string, ceiling string) (sel agent.T3ModeSel, eff t3.Mode, claude string, err error) {
	if sel, err = t.h.Agent.T3Mode(mode); err != nil {
		return
	}
	eff = t3.Clamp(t3.Clamp(t3.Mode(sel.Runtime), t3.Mode(ceiling)), t.Ceiling)
	claude = sel.Claude
	if string(eff) != sel.Runtime {
		claude = agent.T3ClaudeMode(string(eff))
	}
	return
}

func (t *T3Home) start(ctx context.Context, in startIn) (any, error) {
	return t.startOnPlatform(ctx, in, runtime.GOOS)
}

func (t *T3Home) startOnPlatform(ctx context.Context, in startIn, goos string) (any, error) {
	if in.task {
		if err := agenttask.CheckPlatform(goos); err != nil {
			return nil, err
		}
		probe := t.verifyTaskSupervisor
		if t.TaskSupervisor != nil {
			probe = t.TaskSupervisor
		}
		if err := probe(ctx); err != nil {
			return nil, err
		}
	}
	if in.task {
		t.taskMu.Lock()
		defer t.taskMu.Unlock()
	}
	if strings.TrimSpace(in.prompt) == "" {
		return nil, errors.New("the prompt is empty")
	}
	if !agent.OkBot(in.bot) {
		return nil, errors.New("bad bot id")
	}
	if in.role == "" {
		return nil, errors.New("a T3 session runs in a role: say which role (t3-agent.projects lists them)")
	}
	if in.model != "" && !modelRE.MatchString(in.model) {
		return nil, errors.New("bad model name")
	}
	// The role must be active and bound to the configured source directory.
	// The launcher rechecks this binding before every provider process starts.
	r, err := t.h.Agent.T3Role(in.role)
	if err != nil {
		return nil, err
	}
	r.Release()
	if in.project != "" && in.project != r.Project {
		return nil, fmt.Errorf("role %s doesn't belong to project %q", clipStr(r.Name, 40), clipStr(in.project, 64))
	}
	sel, eff, claude, err := t.clampMode(in.mode, r.MaxMode)
	if err != nil {
		return nil, err
	}
	// An unattended task must not silently wait on an unhandled native approval.
	// The single strong task approval includes this mode; existing host/role
	// ceilings still apply and are never raised by a task request.
	if in.task && in.autoFinish && (in.mode != "bypassPermissions" || eff != t3.FullAccess) {
		return nil, errors.New("task: unattended tasks require explicitly approved full-access native mode within srt; this Home or role ceiling does not permit it")
	}
	provider := in.provider
	if provider == "" {
		provider = "claudeAgent"
	}
	if _, ok := agentsandbox.Route(provider); !ok && provider != "cursor" {
		return nil, errors.New("unsupported sandboxed provider")
	}
	if provider != "claudeAgent" && in.model == "" {
		return nil, errors.New("say which model to use for this provider")
	}
	var workspace agenttask.Workspace
	dir := r.ProjectDir
	replay := false
	if in.task {
		if in.publish != nil {
			if err := in.publish.Validate(); err != nil {
				return nil, err
			}
			if in.proof == "" || in.messageID == "" {
				return nil, errors.New("task: publishing requires a core-confirmed target and stable message ID")
			}
		}
		if in.messageID == "" {
			in.messageID = newMsgID()
		}
		if in.model == "" {
			in.model = t.Model
		}
		in.project = r.Project
		in.provider = provider
		spec := in.saved()
		prior, found, e := t.taskStore().Find(ctx, spec)
		if e != nil {
			return nil, e
		}
		replay = found
		if found {
			if prior.Phase != agenttask.Starting {
				return taskStartResult(prior), nil
			}
			workspace, e = agenttask.Resolve(t.h.Dir, prior.ID, r.Project, r.ID, roles.DirHash(r.ProjectDir))
			if e != nil {
				_, _ = t.taskStore().Outcome(ctx, prior.ID, prior.Thread, agenttask.Failed)
				return nil, e
			}
		} else {
			if err := t.checkCaps(r.Project, r.ID, in.bot); err != nil {
				return nil, err
			}
			workspace, e = agenttask.Prepare(ctx, t.h.Dir, r.ProjectDir, r.Project, r.ID)
			if e != nil {
				return nil, e
			}
			if e = t.taskStore().Create(ctx, workspace, spec); e != nil {
				return nil, e
			}
		}
		dir = workspace.Dir
	}
	cl, err := t.readyForTaskProvider(ctx, r.Project, r.ID, provider, "", workspace.ID)
	if err != nil {
		return nil, err
	}
	t.h.Agent.SeeBot(in.bot)
	if !replay {
		if err := t.checkCaps(r.Project, r.ID, in.bot); err != nil {
			return nil, err
		}
	}
	var pid string
	if provider == "cursor" {
		pid, err = t.findOrMakeProject(ctx, cl, dir, r.Project)
	} else {
		pid, err = t.ensureProject(ctx, cl, dir, r.Project)
	}
	if err != nil {
		return nil, err
	}
	mid := in.messageID
	if mid == "" {
		mid = newMsgID()
	}
	title := strings.TrimSpace(in.name)
	if title == "" {
		title = firstLine(in.prompt, 60)
	}
	model := in.model
	if model == "" {
		model = t.Model
	}
	instance := t3vm.InstancePrefix + r.ID
	if provider != "claudeAgent" {
		instance = "rubi-" + strings.ToLower(provider) + "-" + r.ID
	}
	if workspace.ID != "" && provider != "cursor" {
		instance += "-" + workspace.ID
	}
	run, err := cl.Launch(ctx, t3.LaunchIn{ProjectID: pid, Title: title, Instance: instance, Model: model, Mode: eff,
		Plan: sel.Plan, Message: labelled(in.from, in.prompt), RequestID: mid})
	if err != nil {
		return nil, err
	}
	if run.ThreadID == "" {
		return nil, errors.New("T3 started no thread")
	}
	now := t.now().UTC()
	th := t3Thread{ID: run.ThreadID, Project: r.Project, Role: r.ID, Name: title, Bot: in.bot, Mode: claude, Runtime: string(eff),
		Task: workspace.ID, Branch: workspace.Branch, Provider: provider, Environment: environmentFor(provider, dir), Created: now, LastActivity: now, Running: true, RunID: run.RunID, To: toOf(in.from, in.bot), ReplyTo: []string{mid}}
	if old, exists := t.threads.get(th.ID); exists {
		if old.Task != th.Task || old.Role != th.Role || old.Project != th.Project {
			return nil, errors.New("task: native receipt belongs to another session")
		}
		th = old
	} else if err := t.threads.add(th); err != nil {
		_ = cl.Interrupt(ctx, run.ThreadID)
		return nil, err
	}
	if th.Task != "" {
		phase := agenttask.Running
		if !th.Running {
			phase = taskThreadOutcome(th)
		}
		if _, err := t.taskStore().Outcome(ctx, th.Task, th.ID, phase); err != nil {
			return nil, err
		}
	}
	t.emitStatus(th, "busy")
	t.startWatch(th.ID)
	result := map[string]any{"id": th.ID}
	if workspace.ID != "" {
		result["task"], result["branch"], result["base_commit"] = workspace.ID, workspace.Branch, workspace.BaseCommit
	}
	return result, nil
}

func (t *T3Home) sendDirect(ctx context.Context, project, role, id, text, from, bot, messageID string, steer bool) (any, error) {
	th, err := t.ownOpen(project, role, id)
	if err != nil {
		return nil, err
	}
	// Same checks as a runner's send: the role is active and made for the folder as it is now.
	r, err := t.h.Agent.T3Role(th.Role)
	if err != nil {
		return nil, err
	}
	r.Release()
	cl, err := t.readyForThread(ctx, th)
	if err != nil {
		return nil, err
	}
	t.h.Agent.SeeBot(bot)
	if !th.Running {
		if err := t.checkCaps(th.Project, th.Role, th.Bot); err != nil {
			return nil, err
		}
	}
	mid := messageID
	if mid == "" {
		mid = newMsgID()
	}
	mode := ""
	if steer {
		mode = "steer"
	}
	run, err := cl.SendWith(ctx, id, labelled(from, text), mid, mode)
	if err != nil {
		return nil, err
	}
	t.threads.update(id, func(x *t3Thread) {
		x.Running, x.To, x.LastActivity = true, toOf(from, bot), t.now().UTC()
		x.TaskOutcome = ""
		x.TaskReport = ""
		if run.RunID != "" {
			x.RunID = run.RunID
		}
		x.ReplyTo = append(x.ReplyTo, mid)
		if len(x.ReplyTo) > 32 {
			x.ReplyTo = x.ReplyTo[len(x.ReplyTo)-32:]
		}
	})
	if !th.Running {
		t.emitStatus(th, "busy")
	}
	t.startWatch(id)
	return map[string]any{"queued": true, "position": 0, "delivered": run.Delivery != "queued", "id": mid}, nil
}

// ---- rename, fork, interrupt, close, mode ----

func (t *T3Home) rename(ctx context.Context, project, role, id, name string) (any, error) {
	th, err := t.ownOpen(project, role, id)
	if err != nil {
		return nil, err
	}
	cl, err := t.readyForThread(ctx, th)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if err := cl.Rename(ctx, id, name); err != nil {
		return nil, err
	}
	t.threads.update(id, func(x *t3Thread) { x.Name = name })
	return map[string]any{"ok": true}, nil
}

func (t *T3Home) forkDirect(ctx context.Context, project, role, id, name string) (any, error) {
	th, err := t.ownOpen(project, role, id)
	if err != nil {
		return nil, err
	}
	r, err := t.h.Agent.T3Role(th.Role)
	if err != nil {
		return nil, err
	}
	r.Release()
	cl, err := t.readyForThread(ctx, th)
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(name)
	if title == "" {
		title = firstLine(th.Name, 50) + " (fork)"
	}
	nid, err := cl.Fork(ctx, id, title)
	if err != nil {
		return nil, err
	}
	// What the fork already holds is its history: only later items are the turns of this thread.
	pos := 0
	if _, items, err := readAll(ctx, cl, nid, 0); err == nil {
		for _, it := range items {
			pos = max(pos, it.Position)
		}
	}
	now := t.now().UTC()
	f := t3Thread{ID: nid, Project: th.Project, Role: th.Role, Name: title, Bot: th.Bot, Mode: th.Mode, Runtime: th.Runtime,
		Created: now, LastActivity: now, LastPos: pos, Provider: th.Provider, Environment: th.Environment, Task: th.Task, Branch: th.Branch}
	if err := t.threads.add(f); err != nil {
		return nil, err
	}
	return map[string]any{"id": nid}, nil
}

func (t *T3Home) interrupt(ctx context.Context, project, role, id string) (any, error) {
	th, err := t.ownOpen(project, role, id)
	if err != nil {
		return nil, err
	}
	if th.Task != "" {
		return t.stopTask(ctx, th.Task)
	}
	cl, err := t.readyForStop(ctx, th)
	if err != nil {
		return nil, err
	}
	if err := cl.Interrupt(ctx, id); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// closeThread ends Home's use of a thread: its run is interrupted if T3 is there, the watch stops, and the thread is
// no longer listed or accepted by send. T3 has no archive over its tool interface, so the thread stays in T3.
func (t *T3Home) closeThread(ctx context.Context, project, role, id string) (any, error) {
	th, err := t.own(project, role, id)
	if err != nil {
		return nil, err
	}
	if th.Task != "" {
		if _, err := t.stopTask(ctx, th.Task); err != nil {
			return nil, err
		}
	}
	if th.Running {
		if cl, readyErr := t.readyForStop(ctx, th); readyErr == nil {
			if err := cl.Interrupt(ctx, id); err != nil {
				log.Printf("T3 close: interrupting %s: %v", clipStr(id, 40), err)
			}
		}
	}
	t.stopWatch(id)
	t.threads.update(id, func(x *t3Thread) { x.Closed, x.Running, x.RunID = true, false, "" })
	if th.Running {
		t.emitStatus(th, "closed")
	}
	return map[string]any{"ok": true}, nil
}

// setMode is claude-agent.mode. T3 keeps a running thread's permission mode (its tool for changing a thread says
// "permission modes stay"), so the mode can only be confirmed, not changed: the answer is ok when the clamped mode is
// the one the thread has, and a plain error saying so otherwise.
func (t *T3Home) setMode(ctx context.Context, project, role, id, mode string) (any, error) {
	th, err := t.ownOpen(project, role, id)
	if err != nil {
		return nil, err
	}
	r, err := t.h.Agent.T3Role(th.Role)
	if err != nil {
		return nil, err
	}
	r.Release()
	sel, eff, _, err := t.clampMode(mode, r.MaxMode)
	if err != nil {
		return nil, fmt.Errorf("a session id and a valid mode are needed: %v", err)
	}
	cl, err := t.readyForThread(ctx, th)
	if err != nil {
		return nil, err
	}
	head, _, err := cl.Read(ctx, id, th.LastPos)
	if err != nil {
		return nil, err
	}
	havePlan := th.Mode == "plan"
	if string(eff) == head.RuntimeMode && sel.Plan == havePlan && string(eff) == sel.Runtime {
		t.threads.update(id, func(x *t3Thread) { x.Mode = sel.Claude })
		return map[string]any{"ok": true}, nil
	}
	return nil, fmt.Errorf("T3 can't change the permission mode of a session once it has started (this one runs in %s); "+
		"start a new session in the mode you want", agent.T3ClaudeMode(head.RuntimeMode))
}

// ---- the T3 project of a folder ----

func sameFolder(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, ea := evalLink(a)
	rb, eb := evalLink(b)
	return ea == nil && eb == nil && ra == rb
}

// ensureProject finds the T3 project for a folder, or makes it (CreateProject checks that T3 took the folder asked
// for). The folder is the role's pinned project folder, which Home has already vetted.
func (t *T3Home) ensureProject(ctx context.Context, cl *t3.Client, dir, title string) (string, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return "", errors.New("the role's project folder isn't known")
	}
	id, err := t.findOrMakeProject(ctx, cl, dir, title)
	if err != nil {
		return "", err
	}
	// The role's instance is on only in its project's settings, which a project made since the last sync (here, or by
	// the user in T3) doesn't have yet. An unchanged sync writes nothing. Without it the session doesn't start: the
	// role's instance might be off in its project, or still on where it shouldn't be.
	sync := t.syncInstances
	if sync == nil {
		sync = t.h.SyncT3Instances
	}
	sr, err := sync()
	if err != nil {
		log.Printf("T3 role instances: %v", err)
		return "", fmt.Errorf("the session didn't start: Rubi Home couldn't turn the role on in its T3 project (%s)", clipStr(err.Error(), 300))
	}
	if !sr.ProjectsKnown {
		return "", errors.New("the session didn't start: Rubi Home couldn't read T3's projects to turn the role on in its own; try again")
	}
	if sr.Changed() {
		log.Printf("T3 role instances: added %v, updated %v, removed %v", sr.Added, sr.Updated, sr.Removed)
	}
	return id, nil
}

// findOrMakeProject is ensureProject's lookup, one at a time so a folder isn't made twice.
func (t *T3Home) findOrMakeProject(ctx context.Context, cl *t3.Client, dir, title string) (string, error) {
	t.projMu.Lock()
	defer t.projMu.Unlock()
	ps, err := cl.Projects(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range ps {
		if sameFolder(p.WorkspaceRoot, dir) {
			return p.ID, nil
		}
	}
	p, err := cl.CreateProject(ctx, dir, title)
	if err != nil {
		return "", err
	}
	return p.ID, nil
}

// ---- the watch on a running thread: a turn that finishes becomes an event for the plugin ----

func (t *T3Home) emitStatus(th t3Thread, state string) {
	raw, _ := json.Marshal(map[string]any{"session_id": th.ID, "kind": "status", "state": state, "status": state})
	t.h.Agent.Emit(agent.Item{Type: "event", Project: th.Project, Role: th.Role, SessionID: th.ID, Event: raw})
}

func (t *T3Home) stopWatch(id string) {
	t.mu.Lock()
	watch := t.watch[id]
	delete(t.watch, id)
	t.mu.Unlock()
	if watch != nil {
		watch.cancel()
	}
}

// resumeWatchers starts the watch again for the threads that were running when Home last stopped.
func (t *T3Home) resumeWatchers() {
	for _, th := range t.threads.open("", "") {
		if th.Running {
			t.startWatch(th.ID)
		}
	}
}

// startWatch long-polls the thread's run with t3_thread_wait until it ends, then emits the finished-turn item; a
// message sent meanwhile starts another run, which it waits for in turn. One watch per thread.
func (t *T3Home) startWatch(id string) {
	th, _ := t.threads.get(id)
	projectBase := t.baseCtx()
	t.mu.Lock()
	parent := t.runCtx
	if th.Provider == "cursor" {
		parent = projectBase
	}
	if _, on := t.watch[id]; on || parent == nil || parent.Err() != nil || t.watchPaused {
		t.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	watch := &t3Watch{cancel: cancel, done: make(chan struct{})}
	t.watch[id] = watch
	t.watchWG.Add(1)
	t.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			t.mu.Lock()
			if t.watch[id] == watch {
				delete(t.watch, id)
			}
			t.mu.Unlock()
			close(watch.done)
			t.watchWG.Done()
		}()
		t.watchThread(ctx, id)
	}()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func (t *T3Home) watchThread(ctx context.Context, id string) {
	fails := 0
	for ctx.Err() == nil {
		th, ok := t.threads.get(id)
		if !ok || th.Closed || !th.Running {
			return
		}
		cl, readyErr := t.readyForTaskWatch(ctx, th)
		if readyErr != nil {
			return // T3 went away; the next start resumes the watch
		}
		w, err := cl.WaitRun(ctx, id, th.RunID, t3WaitSlice)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if fails++; fails > 20 {
				log.Printf("T3: giving up watching %s: %v", clipStr(id, 40), err)
				return
			}
			if !sleepCtx(ctx, time.Duration(min(fails, 15))*2*time.Second) {
				return
			}
			continue
		}
		fails = 0
		if !w.Done() {
			if !w.TimedOut && !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		if t.finishTurn(ctx, cl, id, w) {
			return
		}
	}
}

// finishTurn emits the item for a run that ended. It reports whether the watch is over (no newer run was sent).
func (t *T3Home) finishTurn(ctx context.Context, cl *t3.Client, id string, w t3.WaitResult) (over bool) {
	if ctx.Err() != nil {
		return true
	}
	th, ok := t.threads.get(id)
	if !ok {
		return true
	}
	text, pos := "", th.LastPos
	if _, items, err := readAll(ctx, cl, id, th.LastPos); err != nil {
		log.Printf("T3: reading %s after its turn: %v", clipStr(id, 40), err)
	} else {
		var any, mine string
		for _, it := range items {
			pos = max(pos, it.Position)
			if it.Type != "assistant_message" {
				continue
			}
			any = it.Text
			if w.RunID != "" && it.RunID == w.RunID {
				mine = it.Text
			}
		}
		if text = mine; text == "" {
			text = any
		}
	}
	if ctx.Err() != nil {
		return true
	}
	truncated := false
	if r := []rune(text); len(r) > maxResultRunes {
		text, truncated = string(r[:maxResultRunes]), true
	}
	if th.Task != "" {
		if state, e := t.taskStore().Get(ctx, th.Task); e == nil && state.Phase == agenttask.Stopped {
			w.Status = "cancelled"
		}
	}
	subtype := w.Status
	if subtype == "completed" {
		subtype = "success"
	}
	reply := th.ReplyTo
	if reply == nil {
		reply = []string{}
	}
	raw, _ := json.Marshal(map[string]any{"session_id": id, "task": th.Task, "branch": th.Branch, "kind": "result", "subtype": subtype, "is_error": w.Status == "failed",
		"text": text, "to": th.To, "in_reply_to": reply, "truncated": truncated, "run_id": w.RunID})
	over = true
	cur, _ := t.threads.update(id, func(x *t3Thread) {
		x.LastActivity = t.now().UTC()
		if x.RunID != "" && x.RunID != w.RunID {
			return // a message sent meanwhile started a newer run: stay on it
		}
		x.LastPos, x.ReplyTo = pos, nil
		x.Running, x.RunID = false, ""
		x.TaskOutcome = w.Status
		if w.Status == "completed" && !truncated {
			x.TaskReport = text
		}
	})
	if th.Task != "" && !cur.Running {
		phase := agenttask.Waiting
		if w.Status == "failed" {
			phase = agenttask.Failed
		} else if w.Status == "cancelled" || w.Status == "interrupted" {
			phase = agenttask.Stopped
		} else {
			for _, other := range t.threads.snapshot() {
				if other.Task == th.Task && other.Running && !other.Closed {
					phase = agenttask.Running
				}
			}
		}
		if state, err := t.taskStore().Outcome(ctx, th.Task, th.ID, phase); err == nil && state.Phase.Terminal() {
			if state.Phase == agenttask.Failed || state.Phase == agenttask.Stopped {
				_, _ = t.stopTask(ctx, state.ID)
			} else {
				t.emitTaskResult(state)
			}
		}
	}
	t.h.Agent.Emit(agent.Item{Type: "event", Project: th.Project, Role: th.Role, SessionID: id, Event: raw})
	if cur.Running {
		over = false
	} else {
		t.emitStatus(cur, "idle")
	}
	return over
}

func environmentFor(provider, dir string) string {
	if provider == "cursor" {
		return roles.DirHash(dir)
	}
	return ""
}
