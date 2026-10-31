package agent

import (
	"encoding/json"
	"sync"
	"testing"
)

// modeBox is a configuration the tests change while the Manager runs.
type modeBox struct {
	mu   sync.Mutex
	conf UsageConfig
}

func (b *modeBox) get() UsageConfig { b.mu.Lock(); defer b.mu.Unlock(); return b.conf }
func (b *modeBox) set(mode string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.conf.Mode = mode
	return nil
}

func usageMods(b *modeBox) func(*Options) {
	return func(o *Options) { o.Usage, o.SetUsageMode = b.get, b.set }
}

func TestPresetLimits(t *testing.T) {
	for mode, want := range map[string]Limits{
		"maximum": {Mode: "maximum", PerProject: 1, Total: 0, Subagents: 0},
		"medium":  {Mode: "medium", PerProject: 1, Total: 4, Subagents: 4},
		"light":   {Mode: "light", PerProject: 1, Total: 1, Subagents: 2},
		"":        {Mode: "medium", PerProject: 1, Total: 4, Subagents: 4},
	} {
		if got := ResolveUsage(UsageConfig{Mode: mode}); got != want {
			t.Errorf("%q: %+v, want %+v", mode, got, want)
		}
	}
}

func TestLightModeQueuesAcrossProjects(t *testing.T) {
	b := &modeBox{conf: UsageConfig{Mode: "light"}}
	mu := newMulti(t, []string{"my-project", "web-app"}, usageMods(b))
	mu.start("my-project", "a")
	r := mu.start("web-app", "b")
	if r["queued"] != true || r["reason"] != "waiting: 1 session is running, the limit" {
		t.Fatalf("light: %v", r)
	}
	mu.fakes["my-project"].Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "queued start ran", func() bool { return mu.startsOf("web-app") == 1 })
}

func TestModeSwitchAppliesToNewStartsOnly(t *testing.T) {
	b := &modeBox{conf: UsageConfig{Mode: "light"}}
	mu := newMulti(t, []string{"my-project", "web-app", "docs-site"}, usageMods(b))
	mu.start("my-project", "a")
	if r := mu.start("web-app", "b"); r["queued"] != true {
		t.Fatalf("light should queue: %v", r)
	}
	// Raising the mode lets the waiting start go at once; the running session is untouched.
	st, err := mu.op("usage_mode", map[string]any{"mode": "medium"})
	if err != nil || st["usage"].(map[string]any)["mode"] != "medium" {
		t.Fatalf("switch: %v %v", st, err)
	}
	eventually(t, "waiting start ran", func() bool { return mu.startsOf("web-app") == 1 })
	if mu.running() != 2 || len(mu.fakes["my-project"].Runner().CallsOf("session.interrupt")) != 0 {
		t.Fatal("a running session was touched")
	}
	// Lowering it kills nothing: both keep running and new work queues.
	if _, err := mu.op("usage_mode", map[string]any{"mode": "light"}); err != nil {
		t.Fatal(err)
	}
	if mu.running() != 2 {
		t.Fatalf("%d running after lowering", mu.running())
	}
	if r := mu.start("docs-site", "c"); r["queued"] != true || r["reason"] != "waiting: 2 sessions are running, the limit" {
		t.Fatalf("new start over the lowered limit: %v", r)
	}
	// The excess finishing is not enough: the queue waits until the count is under the limit.
	mu.fakes["my-project"].Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "one left", func() bool { return mu.running() == 1 })
	if mu.startsOf("docs-site") != 0 {
		t.Fatal("started over the lowered limit")
	}
	mu.fakes["web-app"].Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "queued start ran", func() bool { return mu.startsOf("docs-site") == 1 })
}

func TestUsageModeOpValidatesAndStatusReportsIt(t *testing.T) {
	b := &modeBox{}
	mu := newMulti(t, []string{"my-project"}, usageMods(b))
	if _, err := mu.op("usage_mode", map[string]any{"mode": "turbo"}); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	st, _ := mu.op("status", nil)
	u := st["usage"].(map[string]any)
	l := st["limits"].(map[string]any)
	if u["mode"] != "medium" || u["custom"] != false || l["total"] != float64(4) || l["subagents_per_session"] != float64(4) || len(u["modes"].([]any)) != 3 {
		t.Fatalf("status: %v", st)
	}
	// A raw key that differs from the preset makes it custom.
	one := 1
	b.mu.Lock()
	b.conf.MaxSessions = &one
	b.mu.Unlock()
	st, _ = mu.op("status", nil)
	if st["usage"].(map[string]any)["custom"] != true || st["limits"].(map[string]any)["total"] != float64(1) {
		t.Fatalf("custom: %v", st)
	}
	// Without a way to save, the op refuses.
	none := newMulti(t, []string{"my-project"})
	if _, err := none.op("usage_mode", map[string]any{"mode": "light"}); err == nil {
		t.Fatal("switched without a saver")
	}
}

func TestCapReachesTheRunnerAndSubagentCountShows(t *testing.T) {
	b := &modeBox{conf: UsageConfig{Mode: "light"}}
	mu := newMulti(t, []string{"my-project"}, usageMods(b))
	mu.start("my-project", "a")
	var p struct {
		Max *int `json:"max_subagents"`
	}
	if err := json.Unmarshal(mu.fakes["my-project"].Runner().CallsOf("session.start")[0].Params, &p); err != nil || p.Max == nil || *p.Max != 2 {
		t.Fatalf("start params: %+v %v", p, err)
	}
	mu.fakes["my-project"].Runner().Emit(map[string]any{"session_id": "sess-1", "kind": "subagents", "count": 2})
	eventually(t, "counter", func() bool {
		st, _ := mu.op("status", nil)
		return st["slots"].([]any)[0].(map[string]any)["subagents"] == float64(2) && st["sessions_running"] == float64(1)
	})
}
