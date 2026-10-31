package agent

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// usageBox is a configuration the tests change while the Manager runs; it plays Home's config file.
type usageBox struct {
	mu   sync.Mutex
	conf UsageConfig
}

func (b *usageBox) get() UsageConfig {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.conf
	c.Bots = map[string]string{}
	for k, v := range b.conf.Bots {
		c.Bots[k] = v
	}
	return c
}

func (b *usageBox) set(ch UsageChange) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conf.Bots == nil {
		b.conf.Bots = map[string]string{}
	}
	switch {
	case ch.All:
		b.conf.Default = ch.Mode
		for k := range b.conf.Bots {
			b.conf.Bots[k] = ch.Mode
		}
	case ch.Default:
		b.conf.Default = ch.Mode
	default:
		b.conf.Bots[ch.Bot] = ch.Mode
	}
	return nil
}

func (b *usageBox) see(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conf.Bots == nil {
		b.conf.Bots = map[string]string{}
	}
	b.conf.Bots[id] = b.conf.Default
}

func usageMods(b *usageBox) func(*Options) {
	return func(o *Options) { o.Usage, o.SetUsage, o.SeeBot = b.get, b.set, b.see }
}

func (mu *multi) startAs(bot, project, prompt string) map[string]any {
	mu.t.Helper()
	res, err := mu.op("start", map[string]any{"project": project, "prompt": prompt, "bot": bot})
	if err != nil {
		mu.t.Fatal(err)
	}
	return res
}

func TestPresetLimits(t *testing.T) {
	for mode, want := range map[string]Limits{
		"maximum":  {Mode: "maximum", PerProject: 1, Sessions: 0, Subagents: 0},
		"medium":   {Mode: "medium", PerProject: 1, Sessions: 4, Subagents: 4},
		"light":    {Mode: "light", PerProject: 1, Sessions: 1, Subagents: 2},
		"":         {Mode: "medium", PerProject: 1, Sessions: 4, Subagents: 4},
		"nonsense": {Mode: "medium", PerProject: 1, Sessions: 4, Subagents: 4},
	} {
		if got := (UsageConfig{Default: mode}).For("bot-a"); got != want {
			t.Errorf("%q: %+v, want %+v", mode, got, want)
		}
	}
	// A raw subagent cap overrides every mode and makes it custom; the per-project limit is its own setting.
	five, zero := 5, 0
	l := UsageConfig{Default: "light", MaxSubagents: &five}.For("")
	if l.Subagents != 5 || !l.Custom || l.Sessions != 1 {
		t.Errorf("override: %+v", l)
	}
	if l := (UsageConfig{Default: "medium", MaxSubagents: &zero, MaxSessionsPerProject: 2}).For(""); l.Subagents != 0 || l.PerProject != 2 || !l.Custom {
		t.Errorf("no cap: %+v", l)
	}
	if l := (UsageConfig{Default: "medium", MaxSubagents: new(int)}).For(""); l.Subagents != 0 {
		t.Errorf("zero is no cap: %+v", l)
	}
}

func TestEachBotHasItsOwnLimits(t *testing.T) {
	b := &usageBox{conf: UsageConfig{Bots: map[string]string{"bot-a": "light", "bot-b": "medium"}}}
	mu := newMulti(t, []string{"p1", "p2", "p3", "p4"}, usageMods(b))
	mu.startAs("bot-a", "p1", "x")
	r := mu.startAs("bot-a", "p2", "y")
	if r["queued"] != true || r["reason"] != "waiting: bot bot-a already has 1 session, the limit of its light mode" {
		t.Fatalf("bot-a on light: %v", r)
	}
	// Another Bot on medium is not capped by it.
	for _, p := range []string{"p3", "p4"} {
		if r := mu.startAs("bot-b", p, "z"); r["queued"] == true {
			t.Fatalf("bot-b queued in %s: %v", p, r)
		}
	}
	// bot-a's session ending lets its own waiting start go.
	mu.fakes["p1"].Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "bot-a's queued start", func() bool { return mu.startsOf("p2") == 1 })
}

func TestNewBotsAndTheUserGetTheDefault(t *testing.T) {
	b := &usageBox{conf: UsageConfig{Default: "light"}}
	mu := newMulti(t, []string{"p1", "p2"}, usageMods(b))
	mu.startAs("bot-new", "p1", "x")
	if got := b.get().Bots["bot-new"]; got != "light" {
		t.Fatalf("a new Bot's mode: %q", got)
	}
	if r := mu.startAs("bot-new", "p2", "y"); r["queued"] != true {
		t.Fatalf("new Bot beyond light: %v", r)
	}
	// The user's own sessions (no Bot) use the default too, counted apart from the Bots.
	if r := mu.start("p2", "mine"); r["queued"] == true {
		t.Fatalf("the user's session was capped by a Bot: %v", r)
	}
	// Changing the default later doesn't move a Bot already known.
	if _, err := mu.op("usage_mode", map[string]any{"default": true, "mode": "maximum"}); err != nil {
		t.Fatal(err)
	}
	if b.get().Bots["bot-new"] != "light" || b.get().Default != "maximum" {
		t.Fatalf("config: %+v", b.get())
	}
}

func TestSetModeForAllAndOneByOne(t *testing.T) {
	b := &usageBox{conf: UsageConfig{Bots: map[string]string{"bot-a": "light", "bot-b": "maximum"}}}
	mu := newMulti(t, []string{"p1"}, usageMods(b))
	st, err := mu.op("usage_mode", map[string]any{"all": true, "mode": "medium"})
	if err != nil {
		t.Fatal(err)
	}
	u := st["usage"].(map[string]any)
	if u["default"] != "medium" {
		t.Fatalf("default: %v", u)
	}
	for _, r := range u["bots"].([]any) {
		if r.(map[string]any)["mode"] != "medium" {
			t.Fatalf("row: %v", r)
		}
	}
	// Afterwards one Bot can be changed on its own.
	st, err = mu.op("usage_mode", map[string]any{"bot": "bot-a", "mode": "light"})
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]any{}
	for _, r := range st["usage"].(map[string]any)["bots"].([]any) {
		row := r.(map[string]any)
		modes[row["id"].(string)] = row["mode"]
	}
	if modes["bot-a"] != "light" || modes["bot-b"] != "medium" {
		t.Fatalf("modes: %v", modes)
	}
}

func TestUsageModeOpValidates(t *testing.T) {
	b := &usageBox{}
	mu := newMulti(t, []string{"my-project"}, usageMods(b))
	for _, args := range []map[string]any{
		{"all": true, "mode": "turbo"},
		{"mode": "light"},                              // says nothing about who
		{"bot": "bot-a", "all": true, "mode": "light"}, // says two
		{"bot": "bad id!", "mode": "light"},
	} {
		if _, err := mu.op("usage_mode", args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	if _, err := mu.op("start", map[string]any{"project": "my-project", "prompt": "x", "bot": "bad id!"}); err == nil {
		t.Error("a bad bot id was accepted on start")
	}
	none := newMulti(t, []string{"my-project"})
	if _, err := none.op("usage_mode", map[string]any{"all": true, "mode": "light"}); err == nil {
		t.Fatal("switched without a saver")
	}
}

func TestModeSwitchAppliesToNewStartsOnly(t *testing.T) {
	b := &usageBox{conf: UsageConfig{Default: "light", Bots: map[string]string{"bot-a": "light"}}}
	mu := newMulti(t, []string{"p1", "p2", "p3"}, usageMods(b))
	mu.startAs("bot-a", "p1", "a")
	if r := mu.startAs("bot-a", "p2", "b"); r["queued"] != true {
		t.Fatalf("light should queue: %v", r)
	}
	// Raising the mode lets the waiting start go at once; the running session is untouched.
	if _, err := mu.op("usage_mode", map[string]any{"bot": "bot-a", "mode": "medium"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "waiting start ran", func() bool { return mu.startsOf("p2") == 1 })
	if mu.running() != 2 || len(mu.fakes["p1"].Runner().CallsOf("session.interrupt")) != 0 {
		t.Fatal("a running session was touched")
	}
	// Lowering it kills nothing: both keep running and new work waits.
	if _, err := mu.op("usage_mode", map[string]any{"bot": "bot-a", "mode": "light"}); err != nil {
		t.Fatal(err)
	}
	if mu.running() != 2 {
		t.Fatalf("%d running after lowering", mu.running())
	}
	if r := mu.startAs("bot-a", "p3", "c"); r["queued"] != true || r["reason"] != "waiting: bot bot-a already has 2 sessions, the limit of its light mode" {
		t.Fatalf("new start over the lowered limit: %v", r)
	}
	// One of the two ending is not enough: the count must be under the limit.
	mu.fakes["p1"].Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "one left", func() bool { return mu.running() == 1 })
	if mu.startsOf("p3") != 0 {
		t.Fatal("started over the lowered limit")
	}
	mu.fakes["p2"].Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "queued start ran", func() bool { return mu.startsOf("p3") == 1 })
}

func TestComputerCapHoldsWhateverTheModes(t *testing.T) {
	b := &usageBox{conf: UsageConfig{Default: "maximum"}}
	mu := newMulti(t, []string{"p1", "p2", "p3"}, usageMods(b), func(o *Options) {
		inner := o.Usage
		two := 2
		o.Usage = func() UsageConfig { c := inner(); c.MaxSessions = &two; return c }
	})
	mu.startAs("bot-a", "p1", "x")
	mu.startAs("bot-b", "p2", "y")
	if r := mu.startAs("bot-c", "p3", "z"); r["queued"] != true || r["reason"] != "waiting: the computer's session limit is reached" {
		t.Fatalf("over the computer cap: %v", r)
	}
}

func TestStatusReportsBotsModesAndCounters(t *testing.T) {
	b := &usageBox{conf: UsageConfig{Bots: map[string]string{"bot-a": "light", "bot-idle": "medium"}}}
	mu := newMulti(t, []string{"my-project"}, usageMods(b))
	mu.startAs("bot-a", "my-project", "x")
	mu.fakes["my-project"].Runner().Emit(map[string]any{"session_id": "sess-1", "kind": "subagents", "count": 2})
	eventually(t, "counter", func() bool {
		st, _ := mu.op("status", nil)
		return st["slots"].([]any)[0].(map[string]any)["subagents"] == float64(2)
	})
	st, _ := mu.op("status", nil)
	if st["slots"].([]any)[0].(map[string]any)["bot"] != "bot-a" || st["sessions_running"] != float64(1) || st["limits"].(map[string]any)["total"] != float64(4) {
		t.Fatalf("status: %v", st)
	}
	u := st["usage"].(map[string]any)
	if u["default"] != "medium" || len(u["modes"].([]any)) != 3 {
		t.Fatalf("usage: %v", u)
	}
	rows := map[string]map[string]any{}
	for _, r := range u["bots"].([]any) {
		row := r.(map[string]any)
		rows[row["id"].(string)] = row
	}
	a := rows["bot-a"]
	if a["mode"] != "light" || a["sessions_running"] != float64(1) || a["sessions_limit"] != float64(1) || a["subagents_per_session"] != float64(2) || a["custom"] != false {
		t.Fatalf("bot-a: %v", a)
	}
	if rows["bot-idle"]["sessions_running"] != float64(0) || rows["bot-idle"]["sessions_limit"] != float64(4) {
		t.Fatalf("bot-idle: %v", rows["bot-idle"])
	}
	// A raw override makes the rows custom.
	four := 3
	b.mu.Lock()
	b.conf.MaxSubagents = &four
	b.mu.Unlock()
	st, _ = mu.op("status", nil)
	for _, r := range st["usage"].(map[string]any)["bots"].([]any) {
		if r.(map[string]any)["id"] == "bot-a" && r.(map[string]any)["custom"] != true {
			t.Fatalf("custom: %v", r)
		}
	}
}

func TestBotsCapReachesTheRunner(t *testing.T) {
	b := &usageBox{conf: UsageConfig{Bots: map[string]string{"bot-a": "light", "bot-b": "maximum"}}}
	mu := newMulti(t, []string{"p1", "p2"}, usageMods(b))
	mu.startAs("bot-a", "p1", "x")
	mu.startAs("bot-b", "p2", "y")
	for project, want := range map[string]int{"p1": 2, "p2": 0} {
		var p struct {
			Max *int `json:"max_subagents"`
		}
		if err := json.Unmarshal(mu.fakes[project].Runner().CallsOf("session.start")[0].Params, &p); err != nil || p.Max == nil || *p.Max != want {
			t.Fatalf("%s start params: %+v %v", project, p, err)
		}
	}
}

func TestSessionWithoutASlotIsBookedToItsBot(t *testing.T) {
	b := &usageBox{conf: UsageConfig{Bots: map[string]string{"bot-a": "light"}}}
	mu := newMulti(t, []string{"p1", "p2"}, usageMods(b))
	// bot-a wakes an old session of p2; the slot ends with its turn, but Home remembers whose session it is.
	if r, err := mu.op("send", map[string]any{"project": "p2", "id": "old", "text": "wake", "bot": "bot-a"}); err != nil || r["delivered"] != true {
		t.Fatalf("send: %v %v", r, err)
	}
	mu.fakes["p2"].Runner().Emit(statusEv("old", "idle"))
	eventually(t, "released", func() bool { return mu.running() == 0 })
	mu.startAs("bot-a", "p1", "x") // bot-a is at its limit of one session now
	// The old session runs again without a slot (resumed by the runner): it is booked to bot-a even over the limit.
	mu.fakes["p2"].Runner().Emit(statusEv("old", "busy"))
	eventually(t, "booked", func() bool { return mu.running() == 2 })
	st, _ := mu.op("status", nil)
	bots := map[string]any{}
	for _, sl := range st["slots"].([]any) {
		bots[sl.(map[string]any)["session"].(string)] = sl.(map[string]any)["bot"]
	}
	if bots["old"] != "bot-a" || bots["sess-1"] != "bot-a" {
		t.Fatalf("slots: %v", st["slots"])
	}
	// It is counted: bot-a's next start waits, with the Bot's reason.
	if r := mu.startAs("bot-a", "p2", "more"); r["queued"] != true {
		t.Fatalf("a Bot over its limit started another: %v", r)
	}
	// A session nobody remembers is the user's own.
	mu.fakes["p1"].Runner().Emit(statusEv("stranger", "busy"))
	eventually(t, "stranger booked", func() bool { return mu.running() == 3 })
	for _, r := range func() []any { st, _ := mu.op("status", nil); return st["slots"].([]any) }() {
		if r.(map[string]any)["session"] == "stranger" && r.(map[string]any)["bot"] != "" {
			t.Fatalf("stranger: %v", r)
		}
	}
}

func TestQueueKeepsOrderPerProjectAndBot(t *testing.T) {
	b := &usageBox{conf: UsageConfig{Bots: map[string]string{"bot-a": "light", "bot-b": "maximum"}}}
	mu := newMulti(t, []string{"p1", "p2"}, usageMods(b))
	mu.startAs("bot-a", "p1", "a-run")
	mu.startAs("bot-a", "p2", "a-1") // waits: bot-a is at its limit
	mu.startAs("bot-a", "p2", "a-2")
	mu.startAs("bot-b", "p2", "b-1") // another Bot, free project and limit: it passes bot-a's waiting items
	if mu.startsOf("p2") != 1 {
		t.Fatalf("bot-b's start did not go: %d", mu.startsOf("p2"))
	}
	var first struct{ Prompt string }
	_ = json.Unmarshal(mu.fakes["p2"].Runner().CallsOf("session.start")[0].Params, &first)
	if first.Prompt != "b-1" {
		t.Fatalf("first start in p2: %q", first.Prompt)
	}
	// bot-a's session ends; its items go in order, one project slot at a time.
	mu.fakes["p2"].Runner().Emit(statusEv("sess-1", "idle"))
	mu.fakes["p1"].Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "a-1", func() bool { return mu.startsOf("p2") == 2 })
	var second struct{ Prompt string }
	_ = json.Unmarshal(mu.fakes["p2"].Runner().CallsOf("session.start")[1].Params, &second)
	if second.Prompt != "a-1" || mu.m.q.len() != 1 {
		t.Fatalf("second start in p2: %q, queue %d", second.Prompt, mu.m.q.len())
	}
}

func TestFailedSendToARunningSessionKeepsItsState(t *testing.T) {
	mu := newMulti(t, []string{"my-project"})
	mu.fakes["my-project"].Setup = func(fr *FakeRunner) {
		fr.Handle = func(method string, _ json.RawMessage) (any, error, bool) {
			if method == "session.send" {
				return nil, errors.New("boom"), true
			}
			return nil, nil, false
		}
	}
	mu.start("my-project", "go")
	fr := mu.fakes["my-project"].Runner()
	fr.Emit(map[string]any{"session_id": "sess-1", "kind": "background", "tasks": []any{map[string]any{"task_id": "a1"}}})
	fr.Emit(statusEv("sess-1", "idle"))
	eventually(t, "idle with a subagent", func() bool {
		mu.m.mu.Lock()
		defer mu.m.mu.Unlock()
		return len(mu.m.slots) == 1 && !mu.m.slots[0].turn
	})
	if _, err := mu.op("send", map[string]any{"project": "my-project", "id": "sess-1", "text": "x"}); err == nil {
		t.Fatal("the send did not fail")
	}
	mu.m.mu.Lock()
	defer mu.m.mu.Unlock()
	if len(mu.m.slots) != 1 || mu.m.slots[0].turn {
		t.Fatalf("a failed send left the slot in a turn: %+v", mu.m.slots)
	}
}

func TestIdleTTLSparesOnlyTheSlotWithAnOpenAsk(t *testing.T) {
	var nmu sync.Mutex
	now := time.Now()
	clock := func() time.Time { nmu.Lock(); defer nmu.Unlock(); return now }
	mu := newMulti(t, []string{"my-project", "web-app"}, func(o *Options) { o.Now = clock })
	pl := startPoller(t, mu.m)
	mu.start("my-project", "a")
	mu.start("web-app", "b")
	fr := mu.fakes["my-project"].Runner()
	go func() {
		_, _ = fr.Ask(mu.ctx, AskParams{SessionID: "sess-1", Tool: "Bash", Input: json.RawMessage(`{}`)})
	}()
	pl.wait(t, "perm.ask")
	nmu.Lock()
	now = now.Add(SlotIdleTTL + time.Minute)
	nmu.Unlock()
	mu.m.checkSlot()
	mu.m.mu.Lock()
	left := []string{}
	for _, s := range mu.m.slots {
		left = append(left, s.project)
	}
	mu.m.mu.Unlock()
	if len(left) != 1 || left[0] != "my-project" {
		t.Fatalf("slots left: %v", left)
	}
	// A start still being handed over (no session id yet) gets the delivery time on top.
	mu.m.mu.Lock()
	mu.m.slots = append(mu.m.slots, &slotState{project: "web-app", turn: true, last: now.Add(-SlotIdleTTL - time.Minute)})
	mu.m.mu.Unlock()
	mu.m.checkSlot()
	if mu.running() != 2 {
		t.Fatal("a start in flight was released")
	}
	nmu.Lock()
	now = now.Add(startTimeout)
	nmu.Unlock()
	mu.m.checkSlot()
	if mu.running() != 1 {
		t.Fatalf("a start that never got its id stayed: %d", mu.running())
	}
}
