package core

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/home"
	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/relay/relaytest"
)

// fakeHook stands in for the hook ops of a real Rubi Home: the real hook.install would open the macOS
// administrator dialog.
type fakeHook struct {
	plans, installs atomic.Int32
	planErr         error
	installErr      error
	last            atomic.Value // map[string]any: hello.hook.last
	home            *home.Helper // the real Rubi Home behind the fake ops
}

const fakeSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// pairedFakeHome pairs the core with a real Rubi Home whose hook ops are replaced by f.
func pairedFakeHome(t *testing.T, c *Core, f *fakeHook) string {
	t.Helper()
	return pairedFakeHomeVia(t, c, f, false)
}

// pairedFakeHomeVia is pairedFakeHome; with link the code is the secret-free one of a link and the PIN is typed
// the way a person would (lower case, with the dash).
func pairedFakeHomeVia(t *testing.T, c *Core, f *fakeHook, link bool) string {
	t.Helper()
	rl := relaytest.New()
	t.Cleanup(rl.Close)
	h, err := home.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.home = h
	_ = h.Update(func(cfg *home.Config) { cfg.Relays = []string{rl.URL()} })
	key, _ := h.RelayKey()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan int, 4)
	rpc := &homeproto.Server{Key: h.ID.Box, Handle: func(ctx context.Context, env homeproto.Envelope) (any, error) {
		switch env.Op {
		case "hook.plan":
			f.plans.Add(1)
			if f.planErr != nil {
				return nil, f.planErr
			}
			return map[string]any{"target": "/usr/local/bin/rubi-home", "version": "v1.0.0", "sha256": fakeSHA}, nil
		case "hook.install":
			f.installs.Add(1)
			if f.installErr != nil {
				return nil, f.installErr
			}
			return map[string]any{"started": true}, nil
		case "hello":
			v, err := h.Handle(ctx, env)
			if m, ok := v.(map[string]any); ok && err == nil {
				if l := f.last.Load(); l != nil {
					m["hook"] = map[string]any{"state": "current", "installing": false, "last": l}
				}
			}
			return v, err
		}
		return h.Handle(ctx, env)
	}}
	go (&relay.Server{Key: key, Relays: h.Relays(), Handle: rpc.HandleRPC, Ready: func(n int) { ready <- n }}).Run(ctx)
	<-ready
	code, pin, err := h.PairingCode(false)
	if err != nil {
		t.Fatal(err)
	}
	shown, typed := code.String(), ""
	if link {
		shown, typed = code.WithoutSecret().String(), strings.ToLower(homeproto.FormatPIN(pin))
		if _, err := c.PairDevice(ctx, shown, "AAAA-AAAA"); err == nil {
			t.Fatal("a wrong PIN paired")
		}
	}
	id, err := c.PairDevice(ctx, shown, typed)
	if err != nil {
		t.Fatal(err)
	}
	if snap, err := c.Approvals.DecidePanel(ctx, id, "apply", true, approvals.Verified); err != nil || snap.State != approvals.Executed {
		t.Fatalf("pair: %+v %v", snap, err)
	}
	devs := c.Devices()
	if len(devs) != 1 {
		t.Fatalf("devices: %v", devs)
	}
	return devs[0]["id"].(string)
}

func TestDeviceHookInstallNeedsAnApprovalAndThenStartsItOnHome(t *testing.T) {
	c, _, _ := botCore(t)
	f := &fakeHook{}
	dev := pairedFakeHome(t, c, f)
	ctx := context.Background()

	id, err := c.InstallHookOnDevice(ctx, dev)
	if err != nil || id == "" {
		t.Fatalf("%q %v", id, err)
	}
	snap, err := c.Approvals.Get(id)
	if err != nil || snap.Kind != "rubi.settings" || snap.State != approvals.Pending || snap.Level != approvals.Strong {
		t.Fatalf("%+v %v", snap, err)
	}
	// The approval names what will be installed where, from Home's plan (asked without a prompt).
	det, _ := c.Approvals.Detail(id)
	pv, _ := det.Preview.(map[string]any)
	if f.plans.Load() != 1 || pv["target"] != "/usr/local/bin/rubi-home" || pv["version"] != "v1.0.0" || pv["sha256"] != fakeSHA {
		t.Fatalf("plans %d, preview %v", f.plans.Load(), det.Preview)
	}
	time.Sleep(200 * time.Millisecond)
	if f.installs.Load() != 0 {
		t.Fatal("Home was asked to install before the user approved")
	}
	snap, err = c.Approvals.DecidePanel(ctx, id, "apply", true, approvals.Verified)
	if err != nil || snap.State != approvals.Executed || f.installs.Load() != 1 {
		t.Fatalf("%+v %v installs %d", snap, err, f.installs.Load())
	}
	m, _ := snap.Result.(map[string]any)
	if m["started"] != true || !strings.Contains(m["receipt"].(string), "macOS is asking for the administrator password on ") {
		t.Fatalf("result %v", snap.Result)
	}
	if _, err := c.InstallHookOnDevice(ctx, "nope"); err == nil {
		t.Fatal("an unknown computer was accepted")
	}
	if hookCallTimeout > 45*time.Second {
		t.Fatalf("the call to Home only starts the installation; %s is far too long", hookCallTimeout)
	}
}

func TestDeviceHookInstallRefusedByHomeNeverReachesTheApproval(t *testing.T) {
	c, _, _ := botCore(t)
	f := &fakeHook{planErr: errors.New("the hook is up to date already (up_to_date)")}
	dev := pairedFakeHome(t, c, f)
	id, err := c.InstallHookOnDevice(context.Background(), dev)
	if err == nil || id != "" || !strings.Contains(err.Error(), "up to date") {
		t.Fatalf("%q %v", id, err)
	}
}

func TestDeviceHookInstallFailureReachesTheApproval(t *testing.T) {
	c, _, _ := botCore(t)
	f := &fakeHook{installErr: errors.New("the administrator dialog went unanswered a moment ago; try again in 2 minutes (cooldown)")}
	dev := pairedFakeHome(t, c, f)
	id, err := c.InstallHookOnDevice(context.Background(), dev)
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := c.Approvals.DecidePanel(context.Background(), id, "apply", true, approvals.Verified)
	if snap.State != approvals.Failed || !strings.Contains(snap.Error, "2 minutes") {
		t.Fatalf("%+v", snap)
	}
}

// The audit records a short reason code for each way an installation can end, and once per result.
func TestHookAuditReasonCodes(t *testing.T) {
	for result, want := range map[string][2]string{
		"ok":        {"device.hook.installed", ""},
		"cancelled": {"device.hook.failed", "cancelled"},
		"timeout":   {"device.hook.failed", "timeout"},
		"failed":    {"device.hook.failed", "failed"},
		"made-up":   {"", ""},
	} {
		if ev, reason := hookResultAudit(result); ev != want[0] || reason != want[1] {
			t.Errorf("%s: %q %q", result, ev, reason)
		}
	}
	if hookCallReason(errors.New("the hook is up to date already")) != "refused" || hookCallReason(homeproto.ErrOffline) != "failed" {
		t.Fatal("call reasons")
	}
}

func TestCheckDeviceAuditsAnInstallationResultOnce(t *testing.T) {
	c, _, _ := botCore(t)
	f := &fakeHook{}
	dev := pairedFakeHome(t, c, f)
	count := func(event string) int {
		b, _ := os.ReadFile(c.Layout.Audit())
		return strings.Count(string(b), `"event":"`+event+`"`)
	}
	f.last.Store(map[string]any{"result": "cancelled", "message": "x", "at": float64(1000)})
	for i := 0; i < 3; i++ {
		if _, err := c.CheckDevice(context.Background(), dev); err != nil {
			t.Fatal(err)
		}
	}
	if n := count("device.hook.failed"); n != 1 {
		t.Fatalf("recorded %d times", n)
	}
	f.last.Store(map[string]any{"result": "ok", "message": "x", "at": float64(2000)})
	_, _ = c.CheckDevice(context.Background(), dev)
	_, _ = c.CheckDevice(context.Background(), dev)
	if n := count("device.hook.installed"); n != 1 {
		t.Fatalf("installed recorded %d times", n)
	}
}

func TestPairDeviceWithALinkCodeNeedsThePIN(t *testing.T) {
	c, _, _ := botCore(t)
	dev := pairedFakeHomeVia(t, c, &fakeHook{}, true)
	if dev == "" {
		t.Fatal("not paired")
	}
}

func TestPairDeviceWithoutAPINRefusesASecretFreeCode(t *testing.T) {
	c, _, _ := botCore(t)
	h, err := home.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := h.PairingCode(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PairDevice(context.Background(), code.WithoutSecret().String(), ""); err == nil || !strings.Contains(err.Error(), "PIN") {
		t.Fatalf("err %v", err)
	}
}
