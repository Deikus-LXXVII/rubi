package home

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeRepo makes a repository at dir with an origin remote and a worktree under .claude/worktrees/name
// (the way git lays them out), and returns the worktree's directory.
func fakeRepo(t *testing.T, dir, remote, worktree string) string {
	writeFile(t, filepath.Join(dir, ".git", "config"), "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = "+remote+"\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n")
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	wt := filepath.Join(dir, ".claude", "worktrees", worktree)
	gd := filepath.Join(dir, ".git", "worktrees", worktree)
	writeFile(t, filepath.Join(gd, "HEAD"), "ref: refs/heads/"+worktree+"\n")
	writeFile(t, filepath.Join(gd, "commondir"), "../..\n")
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+gd+"\n")
	return wt
}

func TestNormalizeRemote(t *testing.T) {
	for _, in := range []string{"https://github.com/Deikus-LXXVII/rubi.git", "git@github.com:Deikus-LXXVII/rubi.git",
		"ssh://git@GitHub.com/Deikus-LXXVII/rubi", "https://user:tok@github.com/Deikus-LXXVII/rubi/"} {
		if got := NormalizeRemote(in); got != "github.com/Deikus-LXXVII/rubi" {
			t.Errorf("%s -> %q", in, got)
		}
	}
	for _, in := range []string{"", "/srv/git/x", "nonsense"} {
		if got := NormalizeRemote(in); got != "" {
			t.Errorf("%s -> %q, want nothing", in, got)
		}
	}
}

func TestWorktreeBelongsToItsProject(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	wt := fakeRepo(t, proj, "git@github.com:me/proj.git", "ui")
	main, sub := readGitInfo(proj), readGitInfo(filepath.Join(wt, "src"))
	if main.key != "github.com/me/proj" || sub.key != main.key || sub.project != proj || main.branch != "main" || sub.branch != "ui" {
		t.Fatalf("main %+v sub %+v", main, sub)
	}
	plain := filepath.Join(dir, "plain")
	_ = os.MkdirAll(plain, 0o700)
	if g := readGitInfo(plain); g.key != "local:"+plain {
		t.Fatalf("no git: %+v", g)
	}
}

func TestSessionsFromDesktopTranscriptsAndHooks(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	wt := fakeRepo(t, proj, "https://github.com/me/proj", "ui")
	store := filepath.Join(dir, "store")
	now := time.Now().UTC()
	desk := func(name string, v map[string]any) {
		b, _ := json.Marshal(v)
		writeFile(t, filepath.Join(store, "acct", "org", name), string(b))
	}
	desk("local_a.json", map[string]any{"sessionId": "local_a", "cliSessionId": "cli-a", "cwd": proj, "originCwd": proj,
		"title": "Main\nwork", "lastActivityAt": now.Add(-time.Minute).UnixMilli(), "permissionMode": "default"})
	desk("local_b.json", map[string]any{"sessionId": "local_b", "cliSessionId": "cli-b", "cwd": wt, "originCwd": proj,
		"title": "UI part", "spawnedFrom": map[string]any{"sessionId": "local_a"}, "lastActivityAt": now.Add(-2 * time.Minute).Format(time.RFC3339)})
	desk("local_c.json", map[string]any{"sessionId": "local_c", "cliSessionId": "cli-c", "cwd": proj, "isArchived": true})
	desk("deleted_d.json", map[string]any{"sessionId": "deleted_d", "cliSessionId": "cli-d", "cwd": proj})
	tr := filepath.Join(dir, "projects")
	writeFile(t, filepath.Join(tr, "-x-cli", "cli-e.jsonl"), `{"type":"summary"}`+"\n"+`{"type":"user","cwd":"`+proj+`","gitBranch":"main","sessionId":"cli-e"}`+"\n")
	writeFile(t, filepath.Join(tr, "-x-cli", "cli-a.jsonl"), `{"type":"user","cwd":"`+proj+`"}`+"\n")

	c := &Claude{DesktopStore: store, Transcripts: tr}
	c.Hook([]byte(`{"hook_event_name":"UserPromptSubmit","session_id":"cli-a","cwd":"` + proj + `"}`))
	c.Hook([]byte(`{"hook_event_name":"SessionStart","session_id":"cli-f","cwd":"` + wt + `"}`))

	got := map[string]ClaudeSession{}
	for _, s := range c.Sessions("", false) {
		got[s.SessionID] = s
	}
	if len(got) != 4 {
		t.Fatalf("sessions: %+v", got)
	}
	a, b, e, f := got["cli-a"], got["cli-b"], got["cli-e"], got["cli-f"]
	if a.Source != "desktop" || a.Role != "main" || a.Title != "Main work" || a.RepoKey != "github.com/me/proj" || a.State != "busy" || a.AppSessionID != "local_a" {
		t.Errorf("a: %+v", a)
	}
	if b.Role != "sub" || b.Parent != "local_a" || b.RepoKey != a.RepoKey || b.State != "unknown" || b.LastActivity.IsZero() {
		t.Errorf("b: %+v", b)
	}
	if e.Source != "transcript" || e.Cwd != proj || e.RepoKey != a.RepoKey || e.Branch != "main" {
		t.Errorf("e: %+v", e)
	}
	if f.Source != "hook" || f.Role != "sub" || f.State != "idle" || f.ProjectRoot != proj {
		t.Errorf("f: %+v", f)
	}
	if n := len(c.Sessions("", true)); n != 5 {
		t.Errorf("with archived: %d", n)
	}
	if n := len(c.Sessions("ME/PROJ", false)); n != 4 {
		t.Errorf("filtered: %d", n)
	}
	if n := len(c.Sessions("other", false)); n != 0 {
		t.Errorf("filtered out: %d", n)
	}
}

// A project on a disk that stops answering (git files that can't be read) must not hold up the list: it
// comes back within the budget, that session without git info, and the others with theirs.
func TestHangingDiskDoesNotBlockTheList(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	fakeRepo(t, good, "git@github.com:me/good.git", "x")
	hung := "/Volumes/Hung/proj"
	block := make(chan struct{})
	defer close(block)
	tr := filepath.Join(dir, "projects")
	writeFile(t, filepath.Join(tr, "a", "s-good.jsonl"), `{"cwd":"`+good+`"}`+"\n")
	writeFile(t, filepath.Join(tr, "b", "s-hung.jsonl"), `{"cwd":"`+hung+`"}`+"\n")
	var hungReads atomic.Int32
	r := &sessionRegistry{readGit: func(d string) gitInfo {
		if strings.HasPrefix(d, "/Volumes/Hung") {
			hungReads.Add(1)
			<-block // a blocked open(2): never returns while the disk hangs
		}
		return readGitInfo(d)
	}}
	for round := 0; round < 3; round++ { // the stuck read is reused, not started again each scan
		start := time.Now()
		list := r.list("", tr, time.Now().Add(time.Duration(round)*time.Minute))
		if took := time.Since(start); took > gitScanBudget+time.Second {
			t.Fatalf("round %d took %s", round, took)
		}
		keys := map[string]string{}
		for _, s := range list {
			keys[s.SessionID] = s.RepoKey
		}
		if keys["s-good"] != "github.com/me/good" || keys["s-hung"] != "local:"+hung {
			t.Fatalf("round %d: %v", round, keys)
		}
		if n := hungReads.Load(); n != 1 {
			t.Fatalf("round %d: the hanging directory was read %d times", round, n)
		}
	}
}

// claude-sessions.list answers with an error, not silence, when the listing doesn't finish in time.
func TestSessionsListTimesOut(t *testing.T) {
	old := sessionsListTimeout
	sessionsListTimeout = 100 * time.Millisecond
	defer func() { sessionsListTimeout = old }()
	c := &Claude{}
	c.reg.mu.Lock() // a scan stuck elsewhere holds the registry
	defer c.reg.mu.Unlock()
	start := time.Now()
	_, err := c.handle(context.Background(), "claude-sessions.list", nil)
	if err == nil || !strings.Contains(err.Error(), "too long") || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %s", err, time.Since(start))
	}
}
