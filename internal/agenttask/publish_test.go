package agenttask

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func taskFixture(t *testing.T) (Store, Workspace) {
	t.Helper()
	if CheckPlatform(runtime.GOOS) != nil {
		t.Skip("platform refusal is covered by TestTaskPlatformBoundary")
	}
	home, src := repository(t)
	w, err := Prepare(context.Background(), home, src, "project-a", "role-a")
	if err != nil {
		t.Fatal(err)
	}
	st := Store{Home: home}
	spec := Spec{Supervision: 1, Project: w.Project, Role: w.Role, Prompt: "private prompt", From: "requester", MessageID: "request-one", Target: &Target{Repository: "owner/repo", Base: "main", Account: "fixture"}, Proof: "fake-proof"}
	if err := st.Create(context.Background(), w, spec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "tracked"), []byte("task change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, w.Dir, "add", "tracked")
	runGit(t, w.Dir, "commit", "-m", "task change")
	if _, err := st.Outcome(context.Background(), w.ID, "thread", Waiting); err != nil {
		t.Fatal(err)
	}
	return st, w
}

func TestTaskJournalSurvivesRestartAndTerminalIsImmutable(t *testing.T) {
	st, w := taskFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for _, phase := range []Phase{Failed, Stopped, Finished, Failed, Stopped} {
		wg.Add(1)
		go func(p Phase) {
			defer wg.Done()
			if _, err := st.Outcome(ctx, w.ID, "thread", p); err != nil {
				t.Error(err)
			}
		}(phase)
	}
	wg.Wait()
	first, err := (Store{Home: st.Home}).Get(ctx, w.ID)
	if err != nil || !first.Phase.Terminal() || first.TerminalAt.IsZero() {
		t.Fatal("terminal state not durable", err)
	}
	if _, err := st.Outcome(ctx, w.ID, "other", Running); err != nil {
		t.Fatal(err)
	}
	second, _ := st.Get(ctx, w.ID)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatal("duplicate completion changed a terminal record")
	}
	if err := st.With(ctx, w.ID, func(tx *Tx) error { tx.State.Spec.Target.Repository = "other/repo"; return tx.Save() }); err == nil {
		t.Fatal("pointer mutation bypassed immutable target")
	}
	if first.EventID() != second.EventID() {
		t.Fatal("terminal delivery identity changed")
	}
	public, _ := json.Marshal(first.Public())
	if strings.Contains(string(public), "fake-proof") || strings.Contains(string(public), "private prompt") {
		t.Fatal("public state exposed private mandate")
	}
}
func TestTaskJournalRejectsChangedMandateAndPreservesAmbiguousCommand(t *testing.T) {
	st, w := taskFixture(t)
	ctx := context.Background()
	r, _ := st.Get(ctx, w.ID)
	changed := r.Spec
	changed.Prompt = "different"
	if _, _, err := st.Find(ctx, changed); err == nil {
		t.Fatal("request ID reused with changed prompt")
	}
	if err := st.With(ctx, w.ID, func(tx *Tx) error { tx.State.Spec.Target.Base = "other"; return tx.Save() }); err == nil {
		t.Fatal("shared pointer permitted mandate change")
	}
	if err := st.With(ctx, w.ID, func(tx *Tx) error {
		tx.State.Pending = &Command{Kind: "send", Request: "receipt", Text: "private append"}
		tx.State.Phase = Running
		return tx.Save()
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Outcome(ctx, w.ID, "thread", Waiting); err != nil {
		t.Fatal(err)
	}
	r, _ = st.Get(ctx, w.ID)
	if r.Pending == nil || r.Phase != Running {
		t.Fatal("old completion erased an ambiguous native receipt")
	}
	b, _ := json.Marshal(r.Public())
	if strings.Contains(string(b), "private append") {
		t.Fatal("public pending text not redacted")
	}
}
func TestTaskStatusDoesNotBlockBehindPublication(t *testing.T) {
	st, w := taskFixture(t)
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- st.With(context.Background(), w.ID, func(tx *Tx) error {
			tx.State.Phase = Publishing
			if err := tx.Save(); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := st.Get(ctx, w.ID)
	close(release)
	if err != nil || r.Phase != Publishing {
		t.Fatal("status blocked or stale", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type githubFixture struct {
	mu              sync.Mutex
	requests, posts int
	pr              *pull
	losePost        bool
	failRepo        bool
	wrong           bool
	branch, commit  string
}

func (f *githubFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.Header.Get("Authorization") != "Bearer fixture-not-a-real-token" {
		http.Error(w, "refused", 401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/repos/owner/repo" && r.Method == "GET":
		if f.failRepo {
			http.Error(w, "fixture-not-a-real-token", 503)
			return
		}
		io.WriteString(w, `{"default_branch":"main"}`)
	case r.URL.Path == "/repos/owner/repo/pulls" && r.Method == "GET":
		if r.URL.Query().Get("head") != "owner:"+f.branch || r.URL.Query().Get("base") != "main" || r.URL.Query().Get("state") != "all" {
			http.Error(w, "wrong exact selector", 400)
			return
		}
		if f.pr == nil {
			io.WriteString(w, "[]")
		} else {
			json.NewEncoder(w).Encode([]pull{*f.pr})
		}
	case r.URL.Path == "/repos/owner/repo/pulls" && r.Method == "POST":
		f.posts++
		var in map[string]string
		if json.NewDecoder(r.Body).Decode(&in) != nil || in["head"] != f.branch || in["base"] != "main" || strings.Contains(in["body"], "private prompt") {
			http.Error(w, "wrong PR", 400)
			return
		}
		p := pull{Number: 7, URL: "https://github.com/owner/repo/pull/7"}
		p.Head.Ref = f.branch
		p.Head.SHA = f.commit
		p.Head.Repo.Full = "owner/repo"
		p.Base.Ref = "main"
		if f.wrong {
			p.Head.Ref = "main"
		}
		f.pr = &p
		if f.losePost {
			f.losePost = false
			http.Error(w, "response lost", 503)
			return
		}
		json.NewEncoder(w).Encode(p)
	default:
		http.Error(w, "unexpected API endpoint", 400)
	}
}
func publishFixture(t *testing.T) (Publisher, Workspace, string, *githubFixture) {
	t.Helper()
	st, w := taskFixture(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	if err := os.Mkdir(remote, 0700); err != nil {
		t.Fatal(err)
	}
	runGit(t, remote, "init", "--bare", "-b", "main")
	// Seed only main; the publisher may add exactly the independently assigned ref.
	if _, err := publishGit(context.Background(), w.Dir, "", "", true, "push", "--", remote, w.BaseCommit+":refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	f := &githubFixture{branch: w.Branch, commit: runGit(t, w.Dir, "rev-parse", "HEAD")}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	p := Publisher{Store: st, api: server.URL, client: server.Client(), remote: func(Target) string { return remote }}
	return p, w, remote, f
}
func TestTaskPublishExactBranchBareRemoteAndMockGitHub(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint("lost-response-", lost), func(t *testing.T) {
			p, w, remote, f := publishFixture(t)
			f.losePost = lost
			s, err := p.Publish(context.Background(), w.ID, "fixture-not-a-real-token")
			if lost {
				if err == nil {
					t.Fatal("lost response must remain retryable")
				}
				pending, _ := p.Store.Get(context.Background(), w.ID)
				if pending.Phase != Pushed {
					t.Fatal("push progress lost")
				}
				p.Store = Store{Home: p.Store.Home}
				s, err = p.Publish(context.Background(), w.ID, "fixture-not-a-real-token")
			}
			if err != nil || s.Phase != Finished || s.PR != "https://github.com/owner/repo/pull/7" {
				t.Fatal("publish failed", err)
			}
			oldRevision := s.Revision
			if again, err := p.Publish(context.Background(), w.ID, ""); err != nil || again.Revision != oldRevision {
				t.Fatal("terminal retry changed state", err)
			}
			refs := runGit(t, remote, "for-each-ref", "--format=%(refname)")
			if refs != "refs/heads/main\nrefs/heads/"+w.Branch {
				t.Fatal("foreign ref pushed", refs)
			}
			if runGit(t, remote, "rev-parse", "main") != w.BaseCommit || runGit(t, remote, "rev-parse", w.Branch) != f.commit {
				t.Fatal("exact commit/branch mismatch")
			}
			if f.posts != 1 {
				t.Fatal("duplicate PR")
			}
			_ = filepath.WalkDir(p.Store.Home, func(path string, d os.DirEntry, e error) error {
				if e == nil && !d.IsDir() {
					b, _ := os.ReadFile(path)
					if strings.Contains(string(b), "fixture-not-a-real-token") {
						t.Error("credential persisted")
					}
				}
				return nil
			})
		})
	}
}
func TestTaskPublishRestartUsesFrozenSnapshotNotAgentMutableTree(t *testing.T) {
	p, w, remote, f := publishFixture(t)
	r, _ := p.Store.Get(context.Background(), w.ID)
	// Crash immediately after freezing and before saving the frozen SHA.
	if _, _, err := snapshot(context.Background(), p.Store.Home, r); err != nil {
		t.Fatal(err)
	}
	runGit(t, w.Dir, "config", "core.hooksPath", "/agent-controlled")
	s, err := p.Publish(context.Background(), w.ID, "fixture-not-a-real-token")
	if err != nil || s.Phase != Finished || runGit(t, remote, "rev-parse", w.Branch) != f.commit {
		t.Fatal("frozen recovery touched mutated workspace", err)
	}
}
func TestTaskPublishRefusesRemoteOverwriteAndWrongPRBinding(t *testing.T) {
	for _, scenario := range []string{"foreign-ref", "wrong-pr", "api-error"} {
		t.Run(scenario, func(t *testing.T) {
			p, w, remote, f := publishFixture(t)
			switch scenario {
			case "foreign-ref":
				runGit(t, remote, "update-ref", "refs/heads/"+w.Branch, w.BaseCommit)
			case "wrong-pr":
				f.wrong = true
			case "api-error":
				f.failRepo = true
			}
			_, err := p.Publish(context.Background(), w.ID, "fixture-not-a-real-token")
			if err == nil {
				t.Fatal("unsafe publication accepted")
			}
			if strings.Contains(err.Error(), "fixture-not-a-real-token") {
				t.Fatal("API body or credential leaked")
			}
			if runGit(t, remote, "rev-parse", "main") != w.BaseCommit {
				t.Fatal("main changed")
			}
			r, _ := p.Store.Get(context.Background(), w.ID)
			if r.Phase == Finished {
				t.Fatal("invalid PR finalized")
			}
		})
	}
}
func TestTaskPublishRejectsAgentMetadataAttacksBeforeNetwork(t *testing.T) {
	for _, scenario := range []string{"main-head", "detached", "config", "symlink", "alternates", "no-new-commit", "oversized-head", "oversized-config", "orphan-commit", "corrupt-object", "http-alternates"} {
		t.Run(scenario, func(t *testing.T) {
			p, w, _, f := publishFixture(t)
			switch scenario {
			case "main-head":
				runGit(t, w.Dir, "branch", "-f", "main", "HEAD")
				runGit(t, w.Dir, "symbolic-ref", "HEAD", "refs/heads/main")
			case "detached":
				runGit(t, w.Dir, "checkout", "--detach", "HEAD")
			case "config":
				runGit(t, w.Dir, "config", "core.hooksPath", "/agent-controlled")
			case "symlink":
				if err := os.Symlink("/outside", filepath.Join(w.Dir, ".git", "objects", "bad")); err != nil {
					t.Fatal(err)
				}
			case "alternates":
				if err := os.WriteFile(filepath.Join(w.Dir, ".git", "objects", "info", "alternates"), []byte("/outside\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "no-new-commit":
				runGit(t, w.Dir, "update-ref", "refs/heads/"+w.Branch, w.BaseCommit)
			case "orphan-commit":
				tree := runGit(t, w.Dir, "rev-parse", "HEAD^{tree}")
				orphan := runGit(t, w.Dir, "commit-tree", tree, "-m", "orphan")
				runGit(t, w.Dir, "update-ref", "refs/heads/"+w.Branch, orphan)
			case "corrupt-object":
				commit := runGit(t, w.Dir, "rev-parse", "HEAD")
				object := filepath.Join(w.Dir, ".git", "objects", commit[:2], commit[2:])
				if err := os.Chmod(object, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(object, []byte("invalid compressed object"), 0600); err != nil {
					t.Fatal(err)
				}
			case "http-alternates":
				if err := os.WriteFile(filepath.Join(w.Dir, ".git", "objects", "info", "http-alternates"), []byte("https://outside.example"), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversized-head":
				if err := os.WriteFile(filepath.Join(w.Dir, ".git", "HEAD"), []byte(strings.Repeat("x", 5000)), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversized-config":
				if err := os.WriteFile(filepath.Join(w.Dir, ".git", "config"), []byte(strings.Repeat("x", (1<<20)+1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := p.Publish(context.Background(), w.ID, "fixture-not-a-real-token")
			if err == nil {
				t.Fatal("unsafe snapshot accepted")
			}
			r, _ := p.Store.Get(context.Background(), w.ID)
			if r.Phase != Failed {
				t.Fatal("snapshot failure not terminal")
			}
			revision := r.Revision
			_, _ = p.Publish(context.Background(), w.ID, "fixture-not-a-real-token")
			r, _ = p.Store.Get(context.Background(), w.ID)
			if r.Revision != revision || f.requests != 0 {
				t.Fatal("failed task retried side effects")
			}
			es, _ := os.ReadDir(filepath.Join(p.Store.Home, "t3", "task-snapshots"))
			for _, e := range es {
				if strings.HasPrefix(e.Name(), ".freeze-") {
					t.Fatal("failed snapshot leaked temporary directory")
				}
			}
		})
	}
}
func TestTaskCredentialHelperRefusesOtherHostsAndKeepsTokenInPipe(t *testing.T) {
	for _, request := range []string{"protocol=https\nhost=evil.example\n\n", "protocol=http\nhost=github.com\n\n", "protocol=https\nhost=github.com\n\n"} {
		var out strings.Builder
		err := CredentialHelper("get", strings.NewReader("fixture-not-real"), strings.NewReader(request), &out)
		if strings.Contains(request, "protocol=https\nhost=github.com") {
			if err != nil || !strings.Contains(out.String(), "password=fixture-not-real\n") {
				t.Fatal("private response not delivered")
			}
		} else if err == nil || out.Len() != 0 {
			t.Fatal("credential sent to unapproved destination")
		}
	}
}

func TestTaskPushConditionalCreateRefusesEvenAFastForwardOverExistingRef(t *testing.T) {
	p, w, remote, f := publishFixture(t)
	runGit(t, remote, "update-ref", "refs/heads/"+w.Branch, w.BaseCommit)
	if err := pushBranch(context.Background(), filepath.Join(p.Store.Home, "t3", "workspaces", w.ID), remote, "", "", true, w.Branch, f.commit); err == nil {
		t.Fatal("conditional create replaced existing task ref")
	}
	if runGit(t, remote, "rev-parse", w.Branch) != w.BaseCommit {
		t.Fatal("existing ref changed")
	}
	for _, branch := range []string{"main", "refs/heads/main", "rubi/main", "other/task"} {
		if err := pushBranch(context.Background(), w.Dir, remote, "", "", true, branch, f.commit); err == nil {
			t.Fatal("foreign ref accepted")
		}
	}
}

func TestTaskJournalUnsupportedHostRefusesBeforeStorage(t *testing.T) {
	if CheckPlatform(runtime.GOOS) == nil {
		return
	}
	_, err := (Store{Home: filepath.Join(t.TempDir(), "does-not-exist")}).Get(context.Background(), "task_0123456789abcdef01234567")
	if err == nil || !strings.Contains(err.Error(), "repository tasks are not supported on "+runtime.GOOS) {
		t.Fatal("unsupported host looked up storage before refusing", err)
	}
}

func TestTaskPublishSupportsPackedRefs(t *testing.T) {
	p, w, remote, f := publishFixture(t)
	runGit(t, w.Dir, "pack-refs", "--all", "--prune")
	state, err := p.Publish(context.Background(), w.ID, "fixture-not-a-real-token")
	if err != nil || state.Phase != Finished || runGit(t, remote, "rev-parse", w.Branch) != f.commit {
		t.Fatal("packed task ref not published", err)
	}
}
func TestTaskJournalRejectsUnsafeRecordsAndCancellableLockWait(t *testing.T) {
	st, w := taskFixture(t)
	ctx := context.Background()
	root := filepath.Join(st.Home, "t3", "task-state")
	path := filepath.Join(root, w.ID+".json")
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- st.With(ctx, w.ID, func(tx *Tx) error { close(held); <-release; return nil }) }()
	<-held
	timeout, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	err := st.With(timeout, w.ID, func(*Tx) error { return nil })
	cancel()
	close(release)
	if err == nil || <-done != nil {
		t.Fatal("lock wait ignored cancellation")
	}
	for _, attack := range []string{"wide-permissions", "corrupt", "symlink"} {
		t.Run(attack, func(t *testing.T) {
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Remove(path); _ = os.WriteFile(path, original, 0600) }()
			switch attack {
			case "wide-permissions":
				err = os.Chmod(path, 0666)
			case "corrupt":
				err = os.WriteFile(path, []byte(`{"phase":"finished"}`), 0600)
			case "symlink":
				err = os.Remove(path)
				if err == nil {
					err = os.Symlink(filepath.Join(st.Home, "outside"), path)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.Get(ctx, w.ID); err == nil {
				t.Fatal("unsafe journal accepted")
			}
		})
	}
}

func TestTaskCancelledJournalTransactionNeverExecutes(t *testing.T) {
	st, w := taskFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	if err := st.With(ctx, w.ID, func(tx *Tx) error { ran = true; tx.State.Phase = Finished; return tx.Save() }); err == nil || ran {
		t.Fatal("cancelled transaction executed")
	}
	state, _ := st.Get(context.Background(), w.ID)
	if state.Phase != Waiting {
		t.Fatal("cancelled transaction changed task")
	}
}
