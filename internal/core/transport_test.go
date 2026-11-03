package core

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
)

// fakeDaemon stands in for what the daemon gives core: the stored choice, Tailscale's state, the stop function.
type fakeDaemon struct {
	stored    atomic.Value // string
	saveErr   error
	saves     atomic.Int32
	tailscale atomic.Bool
	stopped   atomic.Bool
}

func (f *fakeDaemon) storedNow() string { v, _ := f.stored.Load().(string); return v }

func transportCore(t *testing.T, current string, tailscale bool) (*Core, *fakeDaemon) {
	t.Helper()
	c, _, _ := botCore(t)
	f := &fakeDaemon{}
	f.stored.Store(current)
	f.tailscale.Store(tailscale)
	c.SetTransportName(current)
	c.SetTransportHooks(TransportHooks{
		Save: func(v string) error {
			f.saves.Add(1)
			if f.saveErr != nil {
				return f.saveErr
			}
			f.stored.Store(v)
			return nil
		},
		Current:        f.storedNow,
		TailscaleReady: f.tailscale.Load,
	})
	c.SetShutdown(func() { f.stopped.Store(true) })
	return c, f
}

func decideTransport(t *testing.T, c *Core, id string) approvals.Snapshot {
	t.Helper()
	snap, err := c.Approvals.DecidePanel(context.Background(), id, "apply", true, approvals.Verified)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestChooseTransportNeedsAnApprovalThenSavesAndRestartsUnlocked(t *testing.T) {
	c, f := transportCore(t, TransportGateway, false)
	ctx := context.Background()
	id, err := c.ChooseTransport(ctx, TransportRelays)
	if err != nil || id == "" {
		t.Fatalf("%q %v", id, err)
	}
	snap, err := c.Approvals.Get(id)
	if err != nil || snap.Kind != "rubi.settings" || snap.State != approvals.Pending || snap.Level != approvals.Strong {
		t.Fatalf("%+v %v", snap, err)
	}
	det, _ := c.Approvals.Detail(id)
	pv, _ := det.Preview.(map[string]any)
	if pv["now"] != TransportGateway || pv["after"] != TransportRelays || !strings.Contains(pv["effect"].(string), "keeps working") {
		t.Fatalf("preview: %v", det.Preview)
	}
	time.Sleep(50 * time.Millisecond)
	if f.saves.Load() != 0 {
		t.Fatal("the choice was stored before the approval")
	}
	if _, ok := c.PendingHandoff(); ok {
		t.Fatal("a restart was planned before the approval")
	}
	snap, err = c.Approvals.DecidePanel(ctx, id, "apply", true, approvals.Verified)
	if err != nil || snap.State != approvals.Executed {
		t.Fatalf("%+v %v", snap, err)
	}
	if f.storedNow() != TransportRelays {
		t.Fatalf("stored %v", f.storedNow())
	}
	// The restart is the update's: into this binary, staying unlocked, for the reason "transport".
	if _, ok := c.PendingHandoff(); !ok || c.HandoffReason() != "transport" {
		t.Fatalf("handoff %v reason %q", ok, c.HandoffReason())
	}
	deadline := time.Now().Add(4 * time.Second)
	for !f.stopped.Load() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !f.stopped.Load() {
		t.Fatal("Rubi was not asked to restart")
	}
}

func TestChooseTransportSaysWhenThePageStopsWorking(t *testing.T) {
	c, _ := transportCore(t, TransportGateway, true)
	id, err := c.ChooseTransport(context.Background(), TransportTailscale)
	if err != nil {
		t.Fatal(err)
	}
	det, _ := c.Approvals.Detail(id)
	pv, _ := det.Preview.(map[string]any)
	eff, _ := pv["effect"].(string)
	if !strings.Contains(eff, "stops working") || !strings.Contains(eff, "new link") || !strings.Contains(eff, "Tailscale") {
		t.Fatalf("a switch that breaks this page doesn't say so: %q", eff)
	}
}

func TestChooseTransportRefusals(t *testing.T) {
	c, f := transportCore(t, TransportGateway, false)
	ctx := context.Background()
	for _, bad := range []string{"", "wifi", "Gateway", "gateway ", "../relays"} {
		if id, err := c.ChooseTransport(ctx, bad); err == nil || id != "" {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := c.ChooseTransport(ctx, TransportGateway); err == nil {
		t.Error("the transport in use was offered again")
	}
	if _, err := c.ChooseTransport(ctx, TransportTailscale); err == nil || !strings.Contains(err.Error(), "Tailscale") {
		t.Errorf("Tailscale without Tailscale: %v", err)
	}
	if f.saves.Load() != 0 {
		t.Fatal("a refused choice was stored")
	}
	c.SetTransportHooks(TransportHooks{})
	if _, err := c.ChooseTransport(ctx, TransportRelays); err == nil {
		t.Error("a Rubi without the daemon's hooks accepted a choice")
	}
}

func TestTransportSettingsShowTheCurrentOneAndWhatIsAvailable(t *testing.T) {
	c, _ := transportCore(t, TransportRelays, false)
	s := c.TransportSettings()
	if s["current"] != TransportRelays {
		t.Fatalf("%v", s)
	}
	avail := map[string]bool{}
	for _, o := range s["options"].([]any) {
		m := o.(map[string]any)
		avail[m["key"].(string)] = m["available"] == true
	}
	if len(avail) != 3 || !avail[TransportGateway] || !avail[TransportRelays] || avail[TransportTailscale] {
		t.Fatalf("options: %v", avail)
	}
	c.SetTransportHooks(TransportHooks{TailscaleReady: func() bool { return true }})
	for _, o := range c.TransportSettings()["options"].([]any) {
		if m := o.(map[string]any); m["key"] == TransportTailscale && m["available"] != true {
			t.Fatal("Tailscale is there but not offered")
		}
	}
}

func TestTailscaleThatIsNotRunningIsNotOfferedAndRefusedAtRequestAndAtExecute(t *testing.T) {
	c, f := transportCore(t, TransportGateway, false)
	ctx := context.Background()
	for _, o := range c.TransportSettings()["options"].([]any) {
		if m := o.(map[string]any); m["key"] == TransportTailscale {
			if m["available"] != false || !strings.Contains(m["help"].(string), "signed in") {
				t.Fatalf("offered, or the help doesn't cover not being signed in: %v", m)
			}
		}
	}
	if _, err := c.ChooseTransport(ctx, TransportTailscale); err == nil || !strings.Contains(err.Error(), "signed in") {
		t.Fatalf("request: %v", err)
	}
	// Ready when asked, gone when the user approves: the execute step looks again, before anything is stored.
	f.tailscale.Store(true)
	id, err := c.ChooseTransport(ctx, TransportTailscale)
	if err != nil {
		t.Fatal(err)
	}
	det, _ := c.Approvals.Detail(id)
	if pv, _ := det.Preview.(map[string]any); !strings.Contains(pv["needs"].(string), "signed in") {
		t.Fatalf("the approval doesn't say Tailscale must be signed in: %v", det.Preview)
	}
	f.tailscale.Store(false)
	snap := decideTransport(t, c, id)
	if snap.State != approvals.Failed || !strings.Contains(snap.Error, "signed in") || f.saves.Load() != 0 {
		t.Fatalf("%+v saves %d", snap, f.saves.Load())
	}
	if _, ok := c.PendingHandoff(); ok {
		t.Fatal("a restart was armed for a refused change")
	}
}

func TestTransportSaveFailsWhenTheProgramCannotBeFoundAndStoresNothing(t *testing.T) {
	c, f := transportCore(t, TransportGateway, false)
	old := executable
	executable = func() (string, error) { return "", errors.New("no path") }
	defer func() { executable = old }()
	id, err := c.ChooseTransport(context.Background(), TransportRelays)
	if err != nil {
		t.Fatal(err)
	}
	snap := decideTransport(t, c, id)
	if snap.State != approvals.Failed || f.saves.Load() != 0 || f.storedNow() != TransportGateway {
		t.Fatalf("%+v saves %d stored %s", snap, f.saves.Load(), f.storedNow())
	}
	if _, ok := c.PendingHandoff(); ok || c.HandoffReason() != "" {
		t.Fatal("a restart was armed")
	}
}

func TestTransportSaveErrorArmsNoRestart(t *testing.T) {
	c, f := transportCore(t, TransportGateway, false)
	f.saveErr = errors.New("disk full")
	id, _ := c.ChooseTransport(context.Background(), TransportRelays)
	snap := decideTransport(t, c, id)
	if snap.State != approvals.Failed || c.HandoffReason() != "" {
		t.Fatalf("%+v reason %q", snap, c.HandoffReason())
	}
	if _, ok := c.PendingHandoff(); ok {
		t.Fatal("a restart was armed after a failed save")
	}
}

func TestTransportWithoutAStopFunctionDoesNotClaimToRestart(t *testing.T) {
	c, f := transportCore(t, TransportGateway, false)
	c.SetShutdown(nil)
	id, _ := c.ChooseTransport(context.Background(), TransportRelays)
	snap := decideTransport(t, c, id)
	m, _ := snap.Result.(map[string]any)
	if snap.State != approvals.Executed || m["restarting"] != false || f.storedNow() != TransportRelays ||
		!strings.Contains(m["receipt"].(string), "restart") {
		t.Fatalf("%+v", snap)
	}
	if restartingSettings(snap) {
		t.Fatal("the agent's approval news would be held back for a restart that isn't coming")
	}
}

// Two approvals made while the choice was different: the second one finds the choice already made.
func TestTwoTransportApprovalsExecutedInSequence(t *testing.T) {
	c, f := transportCore(t, TransportGateway, true)
	a, err := c.ChooseTransport(context.Background(), TransportRelays)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.ChooseTransport(context.Background(), TransportRelays)
	if err != nil {
		t.Fatal(err)
	}
	if s := decideTransport(t, c, a); s.State != approvals.Executed {
		t.Fatalf("%+v", s)
	}
	s := decideTransport(t, c, b)
	if s.State != approvals.Failed || !strings.Contains(s.Error, "already uses") || f.saves.Load() != 1 {
		t.Fatalf("%+v saves %d", s, f.saves.Load())
	}
	// And a different second choice goes ahead, from what was stored (not from what is still in use).
	d, _ := c.ChooseTransport(context.Background(), TransportTailscale)
	if s := decideTransport(t, c, d); s.State != approvals.Executed || f.storedNow() != TransportTailscale {
		t.Fatalf("%+v", s)
	}
}

// An update and a transport change that are both waiting for the one restart: neither is lost, in either order.
func TestRestartReasonsAddUpInEitherOrder(t *testing.T) {
	for _, order := range [][]string{{restartUpdate, restartTransport}, {restartTransport, restartUpdate}} {
		c, _ := transportCore(t, TransportGateway, false)
		for _, r := range order {
			if c.armRestart("/bin/rubi", r) == nil {
				t.Fatal("no stop function")
			}
		}
		if got := c.HandoffReason(); got != "transport,update" {
			t.Errorf("order %v: %q", order, got)
		}
		if exe, ok := c.PendingHandoff(); !ok || exe != "/bin/rubi" {
			t.Errorf("order %v: %q %v", order, exe, ok)
		}
	}
	// a transport approval executed after an update was armed keeps the update's reason
	c, f := transportCore(t, TransportGateway, false)
	c.armRestart("/bin/rubi", restartUpdate)
	id, _ := c.ChooseTransport(context.Background(), TransportRelays)
	decideTransport(t, c, id)
	if c.HandoffReason() != "transport,update" || f.storedNow() != TransportRelays {
		t.Fatalf("%q", c.HandoffReason())
	}
}

// Only the connection approval that armed the restart holds back the agent's news of it.
func TestOnlyTheTransportApprovalHoldsBackItsNews(t *testing.T) {
	c, _ := transportCore(t, TransportGateway, false)
	id, _ := c.ChooseTransport(context.Background(), TransportRelays)
	snap := decideTransport(t, c, id)
	if !restartingSettings(snap) {
		t.Fatalf("%+v", snap)
	}
	// a restart is pending, and some other settings approval ends: it is not held back
	other, err := c.requestSettings(context.Background(), "Something else", map[string]any{"x": "y"},
		func(context.Context) (any, error) { return map[string]any{"applied": true}, nil })
	if err != nil {
		t.Fatal(err)
	}
	os := decideTransport(t, c, other)
	if os.State != approvals.Executed || restartingSettings(os) {
		t.Fatalf("%+v", os)
	}
	// a failed connection approval isn't held back either
	snap.State = approvals.Failed
	if restartingSettings(snap) {
		t.Fatal("a failed change is held back")
	}
}
