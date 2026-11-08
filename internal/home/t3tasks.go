package home

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
	"os"
	"path/filepath"
	"reflect"

	"github.com/Deikus-LXXVII/rubi/internal/agenttask"
	"github.com/Deikus-LXXVII/rubi/internal/t3"
)

func (t *T3Home) taskStore() agenttask.Store { return agenttask.Store{Home: t.h.Dir} }
func (in startIn) saved() agenttask.Spec {
	return agenttask.Spec{Project: in.project, Role: in.role, Prompt: in.prompt, Name: in.name, From: in.from, Mode: in.mode, Bot: in.bot, Model: in.model, MessageID: in.messageID, Provider: in.provider, Target: in.publish, Proof: in.proof}
}
func taskInput(s agenttask.Spec) startIn {
	return startIn{project: s.Project, role: s.Role, prompt: s.Prompt, name: s.Name, from: s.From, mode: s.Mode, bot: s.Bot, model: s.Model, messageID: s.MessageID, provider: s.Provider, task: true, publish: s.Target, proof: s.Proof}
}
func taskStartResult(s agenttask.State) any {
	return map[string]any{"id": s.Thread, "task": s.ID, "branch": s.Workspace.Branch, "phase": s.Phase, "project": s.Spec.Project, "role": s.Spec.Role}
}

func (t *T3Home) send(ctx context.Context, project, role, id, text, from, bot, messageID string, steer bool) (any, error) {
	th, err := t.ownOpen(project, role, id)
	if err != nil {
		return nil, err
	}
	if th.Task == "" {
		return t.sendDirect(ctx, project, role, id, text, from, bot, messageID, steer)
	}
	if messageID == "" {
		messageID = newMsgID()
	}
	cmd := agenttask.Command{Kind: "send", Thread: id, Text: text, From: from, Bot: bot, Request: messageID, Steer: steer}
	return t.taskCommand(ctx, th.Task, cmd, func() (any, error) { return t.sendDirect(ctx, project, role, id, text, from, bot, messageID, steer) })
}
func (t *T3Home) fork(ctx context.Context, project, role, id, name string) (any, error) {
	th, err := t.ownOpen(project, role, id)
	if err != nil {
		return nil, err
	}
	if th.Task == "" {
		return t.forkDirect(ctx, project, role, id, name)
	}
	cmd := agenttask.Command{Kind: "fork", Thread: id, Text: name, Request: newMsgID()}
	return t.taskCommand(ctx, th.Task, cmd, func() (any, error) { return t.forkDirect(ctx, project, role, id, name) })
}
func (t *T3Home) taskCommand(ctx context.Context, id string, cmd agenttask.Command, run func() (any, error)) (out any, err error) {
	err = t.taskStore().With(ctx, id, func(tx *agenttask.Tx) error {
		if tx.State.ID == "" {
			return errors.New("task: missing lifecycle record")
		}
		if !tx.State.Phase.Mutable() || tx.State.Phase == agenttask.Starting {
			return errors.New("task: not ready, sealed or ended; append/fork refused")
		}
		if tx.State.Pending != nil && !reflect.DeepEqual(*tx.State.Pending, cmd) {
			return errors.New("task: previous operation requires recovery")
		}
		tx.State.Pending = &cmd
		tx.State.Phase = agenttask.Running
		if err := tx.Save(); err != nil {
			return err
		}
		var e error
		out, e = run()
		if e != nil {
			return e
		}
		tx.State.Pending = nil
		tx.State.Phase = agenttask.Waiting
		for _, th := range t.threads.snapshot() {
			if th.Task == id && th.Running && !th.Closed {
				tx.State.Phase = agenttask.Running
			}
		}
		return tx.Save()
	})
	return
}
func (t *T3Home) stopTask(ctx context.Context, id string) (any, error) {
	t.taskMu.Lock()
	defer t.taskMu.Unlock()
	err := t.taskStore().With(ctx, id, func(tx *agenttask.Tx) error {
		if tx.State.ID == "" {
			return os.ErrNotExist
		}
		if tx.State.Phase == agenttask.Publishing || tx.State.Phase == agenttask.Pushed {
			return errors.New("task: publication already sealed; stop refused")
		}
		if tx.State.Phase.Terminal() {
			return nil
		}
		if tx.State.Phase == agenttask.Starting && tx.State.Thread == "" {
			for _, th := range t.threads.snapshot() {
				if th.Task == id {
					tx.State.Thread = th.ID
					break
				}
			}
			if tx.State.Thread == "" {
				tx.State.Error = "task: cancellation recorded; native launch outcome unresolved, interruption unconfirmed"
			}
		}
		tx.State.Phase = agenttask.Stopped
		tx.State.Pending = nil
		return tx.Save()
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if state, e := t.taskStore().Get(ctx, id); e != nil && !os.IsNotExist(e) {
		return nil, e
	} else if e == nil && state.Phase == agenttask.Stopped && state.Thread == "" && state.Error != "" {
		t.emitTaskResult(state)
		return nil, errors.New(state.Error)
	}
	var stopErr error
	for _, th := range t.threads.snapshot() {
		if th.Task != id || th.Closed || !th.Running {
			continue
		}
		if th.Provider == "cursor" {
			t.projectMu.Lock()
			server := t.projectServers[th.Environment]
			missing := server == nil || !server.process.Alive()
			t.projectMu.Unlock()
			if missing {
				t.stopWatch(th.ID)
				_, _ = t.threads.update(th.ID, func(v *t3Thread) { v.Running = false; v.TaskOutcome = "interrupted" })
				continue
			}
		}
		cl, e := t.readyForStop(ctx, th)
		if e == nil {
			e = cl.Interrupt(ctx, th.ID)
		}
		if e != nil {
			stopErr = errors.New("task: stop recorded; provider interruption pending, retry")
			continue
		}
		// Keep the watcher: it delivers the native cancelled result to the latest
		// requester and clears Running only when T3 confirms the run has ended.
		t.startWatch(th.ID)
	}
	if stopErr != nil {
		return nil, stopErr
	}
	if state, e := t.taskStore().Get(ctx, id); e == nil && state.Phase.Terminal() {
		t.emitTaskResult(state)
	}
	return map[string]any{"ok": true, "task": id, "stopped": true}, nil
}

// Recovery reuses T3's native clientRequestId receipts. Transfer/fork has no such
// receipt: an interrupted fork is terminally failed rather than duplicated.
func (t *T3Home) recoverTasks(ctx context.Context) {
	states, err := t.taskStore().List(ctx)
	if err != nil {
		return
	}
	for _, s := range states {
		if ctx.Err() != nil {
			return
		}
		if s.Phase == agenttask.Finished {
			t.emitTaskResult(s)
			continue
		}
		if s.Phase == agenttask.Stopped || s.Phase == agenttask.Failed {
			_, _ = t.stopTask(ctx, s.ID)
			continue
		}
		if s.Phase.Mutable() {
			if e := t.validateTaskRole(s); e != nil {
				_ = t.taskStore().With(ctx, s.ID, func(tx *agenttask.Tx) error {
					if !tx.State.Phase.Mutable() {
						return nil
					}
					tx.State.Pending = nil
					tx.State.Phase = agenttask.Failed
					tx.State.Error = "task: project or role binding revoked"
					return tx.Save()
				})
				_, _ = t.stopTask(ctx, s.ID)
				continue
			}
		}
		if s.Phase == agenttask.Starting {
			_, _ = t.start(ctx, taskInput(s.Spec))
			continue
		}
		if !s.Phase.Mutable() {
			continue
		}
		if s.Pending == nil {
			phase := agenttask.Waiting
			found := false
			for _, th := range t.threads.snapshot() {
				if th.Task != s.ID {
					continue
				}
				found = true
				if q := taskThreadOutcome(th); q.Terminal() {
					phase = q
					break
				}
				if th.Running {
					phase = agenttask.Running
				}
			}
			if !found {
				phase = agenttask.Failed
			}
			if phase != s.Phase {
				_, _ = t.taskStore().Outcome(ctx, s.ID, s.Thread, phase)
			}
			continue
		}
		cmd := *s.Pending
		if cmd.Kind == "send" {
			_, _ = t.send(ctx, s.Spec.Project, s.Spec.Role, cmd.Thread, cmd.Text, cmd.From, cmd.Bot, cmd.Request, cmd.Steer)
		} else {
			_ = t.taskStore().With(ctx, s.ID, func(tx *agenttask.Tx) error {
				if !tx.State.Phase.Mutable() {
					return nil
				}
				tx.State.Pending = nil
				tx.State.Phase = agenttask.Failed
				tx.State.Error = "task: native fork outcome unknown after restart"
				return tx.Save()
			})
			_, _ = t.stopTask(ctx, s.ID)
		}
	}
}
func (t *T3Home) handleTask(ctx context.Context, op string, args json.RawMessage) (any, error) {
	var in struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if json.Unmarshal(args, &in) != nil || !agenttask.ValidID(in.ID) {
		return nil, errors.New("task: valid ID required")
	}
	s, err := t.taskStore().Get(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	switch op {
	case "t3-agent.task.inspect":
		return s, nil // encrypted Core broker only; never exposed by plugin broker
	case "t3-agent.task.status":
		return s.Public(), nil
	case "t3-agent.task.stop":
		return t.stopTask(ctx, in.ID)
	case "t3-agent.task.publish":
		if err := t.validateTaskRole(s); err != nil {
			return nil, err
		}
		for _, th := range t.threads.snapshot() {
			if th.Task == s.ID && th.Running && !th.Closed {
				return nil, errors.New("task: wait for all task sessions to become idle")
			}
		}
		helper := ""
		if s.Spec.Target != nil {
			helper, err = t.publisherHelper()
			if err != nil {
				return nil, err
			}
		}
		done, err := (agenttask.Publisher{Store: t.taskStore(), Helper: helper}).Publish(ctx, in.ID, in.Token)
		if err != nil {
			return nil, err
		}
		t.emitTaskResult(done)
		return done.Public(), nil
	default:
		return nil, errors.New("task: unknown operation")
	}
}

func (t *T3Home) publisherHelper() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", errors.New("task: trusted credential helper unavailable")
	}
	return t.publisherHelperFrom(self)
}
func (t *T3Home) publisherHelperFrom(self string) (string, error) {
	c := t.h.Fresh()
	candidate := c.AgentSandbox.AgentExec
	if candidate == "" {
		candidate = filepath.Join(filepath.Dir(self), "rubi-agent-exec")
	}
	p, err := filepath.EvalSymlinks(candidate)
	if err != nil || !filepath.IsAbs(p) {
		return "", errors.New("task: trusted credential helper unavailable")
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Mode()&0111 == 0 || st.Mode().Perm()&0022 != 0 {
		return "", errors.New("task: unsafe credential helper")
	}
	roots := []string{filepath.Join(t.h.Dir, "t3", "sandbox"), filepath.Join(t.h.Dir, "t3", "workspaces")}
	for _, project := range agent.ConfiguredProjects(t.h.Dir, []string{c.T3BaseDir}, c.AgentProjects) {
		roots = append(roots, project.Dir)
	}
	for _, root := range roots {
		if resolved, e := filepath.EvalSymlinks(root); e == nil && agent.Within(p, resolved) {
			return "", errors.New("task: credential helper overlaps agent-writable storage")
		}
	}
	return p, nil
}
func (t *T3Home) emitTaskResult(s agenttask.State) {
	raw, _ := json.Marshal(map[string]any{"kind": "task_result", "event_id": s.EventID(), "task": s.Public(), "from": s.Spec.From, "bot": s.Spec.Bot, "to": toOf(s.Spec.From, s.Spec.Bot), "in_reply_to": []string{s.Spec.MessageID}})
	t.h.Agent.Emit(agent.Item{Type: "event", Project: s.Spec.Project, Role: s.Spec.Role, SessionID: s.Thread, Event: raw})
}

func taskThreadOutcome(th t3Thread) agenttask.Phase {
	if th.Closed || th.TaskOutcome == "interrupted" || th.TaskOutcome == "cancelled" {
		return agenttask.Stopped
	}
	if th.TaskOutcome == "failed" {
		return agenttask.Failed
	}
	return agenttask.Waiting
}

func (t *T3Home) validateTaskRole(s agenttask.State) error {
	r, err := t.h.Agent.T3Role(s.Spec.Role)
	if err != nil {
		return errors.New("task: project or active role binding unavailable")
	}
	defer r.Release()
	if r.Project != s.Spec.Project || roles.DirHash(r.ProjectDir) != s.Workspace.SourceHash {
		return errors.New("task: enrolled project binding changed")
	}
	return nil
}

func (t *T3Home) readyForTaskWatch(ctx context.Context, th t3Thread) (cl *t3.Client, err error) {
	if th.Task == "" {
		return t.readyForThread(ctx, th)
	}
	err = t.taskStore().With(ctx, th.Task, func(tx *agenttask.Tx) error {
		if tx.State.ID == "" {
			return errors.New("task: lifecycle record missing")
		}
		var e error
		if !tx.State.Phase.Mutable() {
			cl, e = t.readyForStop(ctx, th)
		} else {
			cl, e = t.readyForThread(ctx, th)
		}
		return e
	})
	return
}
