package agenttask

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
)

type Phase string

const (
	Starting   Phase = "starting"
	Running    Phase = "running"
	Waiting    Phase = "waiting"
	Publishing Phase = "publishing"
	Pushed     Phase = "pushed"
	Finished   Phase = "finished"
	Failed     Phase = "failed"
	Stopped    Phase = "stopped"
)

func (p Phase) Terminal() bool { return p == Finished || p == Failed || p == Stopped }
func (p Phase) Mutable() bool  { return p == Starting || p == Running || p == Waiting }

type Target struct {
	Repository string `json:"repository"`
	Base       string `json:"base"`
	Account    string `json:"account"`
}

var repoRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
var branchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./-]{0,199}$`)

func (t Target) Validate() error {
	if !repoRE.MatchString(t.Repository) || strings.Contains(t.Repository, "..") || !branchRE.MatchString(t.Base) || strings.Contains(t.Base, "..") || strings.Contains(t.Base, "//") || strings.HasSuffix(t.Base, "/") || strings.HasSuffix(t.Base, ".") || strings.HasSuffix(t.Base, ".lock") || strings.HasPrefix(t.Base, "refs/") || t.Account == "" || len(t.Account) > 256 {
		return errors.New("task: invalid repository, base or credential account")
	}
	return nil
}

type Summary struct {
	Work   string `json:"work,omitempty"`
	Tests  string `json:"tests,omitempty"` // Agent-reported evidence, not independently verified.
	Diff   string `json:"diff,omitempty"`
	Status string `json:"status,omitempty"`
}

type Spec struct {
	Supervision int     `json:"sandbox_supervision,omitempty"`
	AutoFinish  bool    `json:"auto_finish,omitempty"`
	Project     string  `json:"project"`
	Role        string  `json:"role"`
	Prompt      string  `json:"prompt"`
	Name        string  `json:"name,omitempty"`
	From        string  `json:"from"`
	Mode        string  `json:"mode,omitempty"`
	Bot         string  `json:"bot,omitempty"`
	Model       string  `json:"model,omitempty"`
	MessageID   string  `json:"message_id"`
	Provider    string  `json:"provider,omitempty"`
	Target      *Target `json:"publish,omitempty"`
	Proof       string  `json:"publish_proof,omitempty"`
}
type Command struct {
	Kind    string `json:"kind"`
	Thread  string `json:"thread"`
	Text    string `json:"text,omitempty"`
	From    string `json:"from,omitempty"`
	Bot     string `json:"bot,omitempty"`
	Request string `json:"request"`
	Steer   bool   `json:"steer,omitempty"`
}
type State struct {
	ResultDelivered bool `json:"result_delivered,omitempty"`
	// Presentation-only acknowledgement, derived from trusted launcher receipts.
	ExecutionStopped bool      `json:"execution_stopped,omitempty"`
	Summary          Summary   `json:"summary"`
	Version          int       `json:"version"`
	ID               string    `json:"id"`
	Workspace        Workspace `json:"workspace"`
	Spec             Spec      `json:"spec"`
	Phase            Phase     `json:"phase"`
	Thread           string    `json:"thread,omitempty"`
	Pending          *Command  `json:"pending,omitempty"`
	Commit           string    `json:"commit,omitempty"`
	PR               string    `json:"pr,omitempty"`
	Error            string    `json:"error,omitempty"`
	Revision         uint64    `json:"revision"`
	Updated          time.Time `json:"updated"`
	TerminalAt       time.Time `json:"terminal_at,omitempty"`
}

func (s State) Public() State {
	s.Spec.Proof = ""
	s.Spec.Prompt = ""
	if s.Pending != nil {
		p := *s.Pending
		p.Text = ""
		s.Pending = &p
	}
	return s
}
func (s State) EventID() string { return s.ID + ":terminal" }
func (s State) Validate() error {
	if s.Version != 1 || !ValidID(s.ID) || s.Workspace.ID != s.ID || s.Workspace.Branch != "rubi/"+s.ID || s.Spec.Project != s.Workspace.Project || s.Spec.Role != s.Workspace.Role || s.Spec.MessageID == "" {
		return errors.New("task: invalid lifecycle binding")
	}
	switch s.Phase {
	case Starting, Running, Waiting, Publishing, Pushed, Finished, Failed, Stopped:
	default:
		return errors.New("task: invalid lifecycle state")
	}
	if s.Spec.Target != nil {
		return s.Spec.Target.Validate()
	}
	return nil
}

// Store locks across processes as well as goroutines. Home already excludes a
// second daemon, but file locks also protect recovery and independent callers.
type Store struct{ Home string }

func (s Store) root() (string, error) {
	if err := CheckPlatform(runtime.GOOS); err != nil {
		return "", err
	}
	home, err := filepath.EvalSymlinks(s.Home)
	if err != nil || !filepath.IsAbs(home) {
		return "", errors.New("task: Home directory missing")
	}
	for _, p := range []string{filepath.Join(home, "t3"), filepath.Join(home, "t3", "task-state")} {
		if err := privateDir(p); err != nil {
			return "", err
		}
	}
	return filepath.Join(home, "t3", "task-state"), nil
}

type Tx struct {
	State    State
	path     string
	previous State
}

func (t *Tx) Save() error {
	if err := t.State.Validate(); err != nil {
		return err
	}
	if t.previous.Phase.Terminal() {
		a, _ := json.Marshal(t.previous)
		b, _ := json.Marshal(t.State)
		if string(a) != string(b) {
			return errors.New("task: ended task state cannot change")
		}
		return nil
	}
	if t.previous.ID != "" && (t.State.ID != t.previous.ID || t.State.Workspace != t.previous.Workspace || !sameSpec(t.State.Spec, t.previous.Spec)) {
		return errors.New("task: immutable mandate changed")
	}
	t.State.Revision = t.previous.Revision + 1
	t.State.Updated = time.Now().UTC()
	if t.State.Phase.Terminal() && t.State.TerminalAt.IsZero() {
		t.State.TerminalAt = t.State.Updated
	}
	b, err := json.Marshal(t.State)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(t.path), ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), t.path); err != nil {
		return err
	}
	d, e := os.Open(filepath.Dir(t.path))
	if e != nil {
		return e
	}
	err = d.Sync()
	err = errors.Join(err, d.Close())
	if err == nil {
		t.previous = cloneState(t.State)
	}
	return err
}
func sameSpec(a, b Spec) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func (s Store) With(ctx context.Context, id string, fn func(*Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !ValidID(id) {
		return errors.New("task: invalid task id")
	}
	root, err := s.root()
	if err != nil {
		return err
	}
	lock := filepath.Join(root, id+".lock")
	if st, e := os.Lstat(lock); e == nil && (!st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0) {
		return errors.New("task: unsafe journal lock")
	}
	f, err := fsx.OpenNoFollow(lock, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		if err = fsx.TryLock(f); err == nil {
			break
		} else if !errors.Is(err, fsx.ErrLocked) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer fsx.Unlock(f)
	if err := ctx.Err(); err != nil {
		return err
	}
	tx := &Tx{path: filepath.Join(root, id+".json")}
	st, err := os.Lstat(tx.path)
	if err == nil {
		if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 2<<20 {
			return errors.New("task: unsafe lifecycle record")
		}
		b, e := os.ReadFile(tx.path)
		if e != nil {
			return e
		}
		if json.Unmarshal(b, &tx.State) != nil || tx.State.Validate() != nil || tx.State.ID != id {
			return errors.New("task: corrupt lifecycle record")
		}
		tx.previous = cloneState(tx.State)
	} else if !os.IsNotExist(err) {
		return err
	}
	return fn(tx)
}
func (s Store) Get(ctx context.Context, id string) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if !ValidID(id) {
		return State{}, errors.New("task: invalid task ID")
	}
	root, err := s.root()
	if err != nil {
		return State{}, err
	}
	f, err := fsx.OpenNoFollow(filepath.Join(root, id+".json"), os.O_RDONLY|fsx.NonBlock, 0)
	if err != nil {
		return State{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 2<<20 {
		return State{}, errors.New("task: unsafe lifecycle record")
	}
	b, err := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	if err != nil || len(b) > 2<<20 {
		return State{}, errors.New("task: unsafe lifecycle record")
	}
	var s1 State
	if json.Unmarshal(b, &s1) != nil || s1.Validate() != nil || s1.ID != id {
		return State{}, errors.New("task: corrupt lifecycle record")
	}
	return s1, nil
}
func (s Store) List(ctx context.Context) (out []State, err error) {
	root, err := s.root()
	if err != nil {
		return nil, err
	}
	es, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		r, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return
}
func (s Store) Find(ctx context.Context, spec Spec) (State, bool, error) {
	records, err := s.List(ctx)
	if err != nil {
		return State{}, false, err
	}
	for _, r := range records {
		if r.Spec.Project == spec.Project && r.Spec.Role == spec.Role && r.Spec.MessageID == spec.MessageID {
			if !sameSpec(r.Spec, spec) {
				return State{}, false, errors.New("task: request ID reused with different arguments")
			}
			return r, true, nil
		}
	}
	return State{}, false, nil
}
func (s Store) Create(ctx context.Context, w Workspace, spec Spec) error {
	w.Dir = ""
	return s.With(ctx, w.ID, func(tx *Tx) error {
		if tx.State.ID != "" {
			return errors.New("task: lifecycle already exists")
		}
		tx.State = State{Version: 1, ID: w.ID, Workspace: w, Spec: spec, Phase: Starting}
		return tx.Save()
	})
}
func (s Store) Outcome(ctx context.Context, id, thread string, status Phase) (State, error) {
	var out State
	err := s.With(ctx, id, func(tx *Tx) error {
		if tx.State.ID == "" {
			return os.ErrNotExist
		}
		if tx.State.Pending != nil || tx.State.Phase.Terminal() || tx.State.Phase == Publishing || tx.State.Phase == Pushed {
			out = tx.State
			return nil
		}
		if tx.State.Thread == "" {
			tx.State.Thread = thread
		}
		tx.State.Phase = status
		tx.State.Pending = nil
		if err := tx.Save(); err != nil {
			return err
		}
		out = tx.State
		return nil
	})
	return out, err
}
func cloneState(s State) State {
	b, _ := json.Marshal(s)
	var out State
	_ = json.Unmarshal(b, &out)
	return out
}
