package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// multi is a Manager over several projects, each with its own fake VM.
type multi struct {
	*env
	fakes map[string]*Fake
}

func newMulti(t *testing.T, names []string, mods ...func(*Options)) *multi {
	t.Helper()
	root := t.TempDir()
	mu := &multi{env: &env{t: t, dir: filepath.Join(root, "helper")}, fakes: map[string]*Fake{}}
	if err := os.MkdirAll(mu.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, n := range names {
		d := filepath.Join(root, n)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, d)
		mu.fakes[n] = &Fake{}
	}
	mu.ctx, mu.cancel = context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(mu.cancel)
	mu.opt = Options{Dir: mu.dir, Projects: func() []string { return dirs },
		GitHosts: func() []string { return []string{"git.example.com"} },
		NewVM:    func(p Project) VM { return mu.fakes[p.ID] }, Wait: 5 * time.Second}
	for _, f := range mods {
		f(&mu.opt)
	}
	mu.m = New(mu.opt)
	t.Cleanup(func() { mu.m.Close() })
	return mu
}

func (mu *multi) start(project, prompt string) map[string]any {
	mu.t.Helper()
	res, err := mu.op("start", map[string]any{"project": project, "prompt": prompt})
	if err != nil {
		mu.t.Fatal(err)
	}
	return res
}

func (mu *multi) running() int {
	mu.m.mu.Lock()
	defer mu.m.mu.Unlock()
	return len(mu.m.slots)
}

func (mu *multi) startsOf(project string) int {
	fr := mu.fakes[project].Runner()
	if fr == nil {
		return 0
	}
	return len(fr.CallsOf("session.start"))
}

// setLimits sets the per-project limit and the cap on the computer; the Bots' modes don't limit (maximum).
func setLimits(per, total int) func(*Options) {
	return func(o *Options) {
		o.Usage = func() UsageConfig {
			return UsageConfig{Default: ModeMaximum, MaxSessionsPerProject: per, MaxSessions: &total}
		}
	}
}

func TestTwoProjectsRunConcurrently(t *testing.T) {
	mu := newMulti(t, []string{"my-project", "web-app"})
	if r := mu.start("my-project", "a"); r["id"] != "sess-1" {
		t.Fatalf("first: %v", r)
	}
	if r := mu.start("web-app", "b"); r["id"] != "sess-1" || r["queued"] == true {
		t.Fatalf("a second project waited: %v", r)
	}
	eventually(t, "two slots", func() bool { return mu.running() == 2 })
	st, _ := mu.op("status", nil)
	slots := st["slots"].([]any)
	if len(slots) != 2 || st["limits"].(map[string]any)["per_project"] != float64(1) || st["limits"].(map[string]any)["total"] != float64(16) {
		t.Fatalf("status: %v", st)
	}
	first := slots[0].(map[string]any)
	if first["project"] != "my-project" || first["session"] != "sess-1" || first["busy"] != true {
		t.Fatalf("slot: %v", first)
	}
	// The old single "slot" is the first busy one.
	if st["slot"].(map[string]any)["project"] != "my-project" || st["slot"].(map[string]any)["busy"] != true {
		t.Fatalf("compat slot: %v", st["slot"])
	}
	ps, _ := mu.op("projects", nil)
	for _, r := range ps["projects"].([]any) {
		row := r.(map[string]any)
		if row["sessions_running"] != float64(1) || row["slot_busy"] != true {
			t.Fatalf("project row: %v", row)
		}
	}
}

func TestNoSlotStatusKeepsOldField(t *testing.T) {
	mu := newMulti(t, []string{"my-project"})
	st, _ := mu.op("status", nil)
	slot := st["slot"].(map[string]any)
	if slot["busy"] != false || slot["project"] != "" || len(st["slots"].([]any)) != 0 {
		t.Fatalf("idle status: %v", st)
	}
	ps, _ := mu.op("projects", nil)
	row := ps["projects"].([]any)[0].(map[string]any)
	if row["slot_busy"] != false || row["sessions_running"] != float64(0) {
		t.Fatalf("idle project: %v", row)
	}
}

func TestSecondSessionInProjectWaitsThenStarts(t *testing.T) {
	mu := newMulti(t, []string{"my-project"})
	mu.start("my-project", "first")
	r := mu.start("my-project", "second")
	want := "waiting: project my-project already has 1 session"
	if r["queued"] != true || r["reason"] != want {
		t.Fatalf("second: %v", r)
	}
	st, _ := mu.op("status", nil)
	if q := st["queue"].([]any); len(q) != 1 || q[0].(map[string]any)["reason"] != want {
		t.Fatalf("queue status: %v", st["queue"])
	}
	pl := startPoller(t, mu.m)
	_ = pl
	mu.fakes["my-project"].Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "second started", func() bool { return mu.startsOf("my-project") == 2 })
	eventually(t, "queue empty", func() bool { return mu.m.q.len() == 0 })

	// Closing the running session frees the slot too.
	mu.start("my-project", "third")
	if _, err := mu.op("close", map[string]any{"project": "my-project", "id": "sess-2"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "third started", func() bool { return mu.startsOf("my-project") == 3 })
}

func TestQueuedItemCarriesTheReason(t *testing.T) {
	mu := newMulti(t, []string{"my-project"})
	pl := startPoller(t, mu.m)
	mu.start("my-project", "first")
	mu.start("my-project", "second")
	it := pl.wait(t, "queue")
	if it.Action != "queued" || it.Reason != "waiting: project my-project already has 1 session" {
		t.Fatalf("queue item: %+v", it)
	}
}

func TestSeveralSessionsPerProject(t *testing.T) {
	mu := newMulti(t, []string{"my-project"}, setLimits(2, 4))
	a, b := mu.start("my-project", "a"), mu.start("my-project", "b")
	if a["id"] != "sess-1" || b["id"] != "sess-2" {
		t.Fatalf("starts: %v %v", a, b)
	}
	if c := mu.start("my-project", "c"); c["queued"] != true || c["reason"] != "waiting: project my-project already has 2 sessions" {
		t.Fatalf("third: %v", c)
	}
	fr := mu.fakes["my-project"].Runner()
	// An event for one session leaves the other's slot alone.
	fr.Emit(statusEv("sess-2", "idle"))
	eventually(t, "third started", func() bool { return mu.startsOf("my-project") == 3 })
	mu.m.mu.Lock()
	defer mu.m.mu.Unlock()
	if mu.m.exactLocked("my-project", "sess-1") == nil || mu.m.exactLocked("my-project", "sess-3") == nil || len(mu.m.slots) != 2 {
		t.Fatalf("slots: %+v", mu.m.slots)
	}
}

func TestGlobalCapWaitsAndReleases(t *testing.T) {
	mu := newMulti(t, []string{"my-project", "web-app", "docs-site"}, setLimits(1, 2))
	mu.start("my-project", "a")
	mu.start("web-app", "b")
	r := mu.start("docs-site", "c")
	if r["queued"] != true || r["reason"] != "waiting: 2 sessions are running on this computer, the limit" {
		t.Fatalf("over the cap: %v", r)
	}
	mu.fakes["web-app"].Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "waiting start delivered", func() bool { return mu.startsOf("docs-site") == 1 })
	if mu.running() != 2 {
		t.Fatalf("%d running", mu.running())
	}
}

func TestZeroMeansNoGlobalCapAndDefaults(t *testing.T) {
	names := []string{"p1", "p2", "p3", "p4", "p5", "p6"}
	mu := newMulti(t, names, setLimits(1, 0))
	for _, n := range names {
		if r := mu.start(n, "x"); r["queued"] == true {
			t.Fatalf("%s queued with no cap: %v", n, r)
		}
	}
	if mu.running() != len(names) {
		t.Fatalf("%d running", mu.running())
	}
	// The defaults: medium for everyone (4 sessions per Bot), 16 on the computer, 1 per project.
	c := UsageConfig{}
	if l := c.For("bot-a"); l.PerProject != 1 || l.Sessions != 4 || l.Subagents != 4 || c.Machine() != 16 {
		t.Fatalf("defaults %+v %d", l, c.Machine())
	}
	neg := -3
	c = UsageConfig{MaxSessions: &neg, MaxSubagents: &neg}
	if l := c.For(""); l.PerProject != 1 || l.Subagents != 4 || c.Machine() != 16 {
		t.Fatalf("out of range %+v %d", l, c.Machine())
	}
	// The default mode (medium) lets a Bot run 4 sessions.
	mu4 := newMulti(t, names)
	for i, n := range names {
		r := mu4.start(n, "x")
		if (r["queued"] == true) != (i >= 4) {
			t.Fatalf("%s: %v", n, r)
		}
	}
}

func TestRunnerEndReleasesOnlyItsProject(t *testing.T) {
	mu := newMulti(t, []string{"my-project", "web-app"}, setLimits(1, 2))
	mu.start("my-project", "a")
	mu.start("web-app", "b")
	mu.start("my-project", "queued") // waits for its project
	mu.fakes["web-app"].Runner().Exit()
	eventually(t, "web-app slot freed", func() bool {
		mu.m.mu.Lock()
		defer mu.m.mu.Unlock()
		return len(mu.m.slots) == 1 && mu.m.slots[0].project == "my-project"
	})
	if mu.startsOf("my-project") != 1 {
		t.Fatal("the queued start ran while its project was full")
	}
	// A crash of the project's own runner frees its slot and lets the queued start go to a new runner.
	mu.fakes["my-project"].Runner().Exit()
	eventually(t, "queued start ran after the crash", func() bool { return mu.fakes["my-project"].Starts() == 2 && mu.startsOf("my-project") == 1 })
}

func TestIdleTTLIsPerSlot(t *testing.T) {
	var nmu sync.Mutex
	now := time.Now()
	clock := func() time.Time { nmu.Lock(); defer nmu.Unlock(); return now }
	mu := newMulti(t, []string{"my-project", "web-app"}, func(o *Options) { o.Now = clock })
	mu.start("my-project", "a")
	nmu.Lock()
	now = now.Add(SlotIdleTTL - time.Minute)
	nmu.Unlock()
	mu.start("web-app", "b")
	nmu.Lock()
	now = now.Add(2 * time.Minute) // my-project is past the TTL, web-app is not
	nmu.Unlock()
	mu.m.checkSlot()
	mu.m.mu.Lock()
	left := len(mu.m.slots)
	var project string
	if left == 1 {
		project = mu.m.slots[0].project
	}
	mu.m.mu.Unlock()
	if left != 1 || project != "web-app" {
		t.Fatalf("slots left: %d %q", left, project)
	}
	if n := len(mu.fakes["my-project"].Runner().CallsOf("session.interrupt")); n != 1 {
		t.Fatalf("%d interrupts", n)
	}
	if n := len(mu.fakes["web-app"].Runner().CallsOf("session.interrupt")); n != 0 {
		t.Fatalf("the live session was interrupted %d times", n)
	}
}

func TestQueueSkipsAnItemWaitingOnAFullProject(t *testing.T) {
	mu := newMulti(t, []string{"my-project", "web-app"}, setLimits(1, 4))
	mu.start("my-project", "a1")
	mu.start("web-app", "b1")
	mu.start("my-project", "a2") // waits for my-project, first in line
	mu.start("web-app", "b2")    // waits for web-app
	mu.start("my-project", "a3") // behind a2
	// web-app frees: b2 goes though a2 is ahead of it.
	mu.fakes["web-app"].Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "b2 delivered", func() bool { return mu.startsOf("web-app") == 2 })
	if mu.startsOf("my-project") != 1 || mu.m.q.len() != 2 {
		t.Fatalf("my-project starts %d, queue %d", mu.startsOf("my-project"), mu.m.q.len())
	}
	// my-project frees: a2 before a3.
	mu.fakes["my-project"].Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "a2 delivered", func() bool { return mu.startsOf("my-project") == 2 })
	time.Sleep(50 * time.Millisecond)
	if mu.startsOf("my-project") != 2 || mu.m.q.len() != 1 {
		t.Fatal("a3 overtook a2 or ran early")
	}
	var p struct{ Prompt string }
	if err := json.Unmarshal(mu.fakes["my-project"].Runner().CallsOf("session.start")[1].Params, &p); err != nil || p.Prompt != "a2" {
		t.Fatalf("second delivered: %q %v", p.Prompt, err)
	}
}

func TestSendToANewSessionTakesASlotAndAnOwnersSendDoesNot(t *testing.T) {
	mu := newMulti(t, []string{"my-project"}, setLimits(2, 4))
	mu.start("my-project", "a")
	if r, err := mu.op("send", map[string]any{"project": "my-project", "id": "sess-1", "text": "more"}); err != nil || r["delivered"] != true {
		t.Fatalf("owner send: %v %v", r, err)
	}
	if r, err := mu.op("send", map[string]any{"project": "my-project", "id": "old", "text": "wake"}); err != nil || r["delivered"] != true {
		t.Fatalf("send to an idle session with a free slot: %v %v", r, err)
	}
	if mu.running() != 2 {
		t.Fatalf("%d running", mu.running())
	}
	if r, _ := mu.op("send", map[string]any{"project": "my-project", "id": "older", "text": "wake"}); r["delivered"] != false || r["queued"] != true {
		t.Fatalf("third session over the limit: %v", r)
	}
}
