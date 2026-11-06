package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/core"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/relay/relaytest"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

func TestTransportChoice(t *testing.T) {
	l := paths.Layout{Home: t.TempDir()}
	if got := LoadTransport(l).Transport; got != TransportGateway {
		t.Fatalf("default: %s", got)
	}
	if err := SaveTransport(l, "carrier-pigeon"); err == nil {
		t.Fatal("unknown transport saved")
	}
	for _, tr := range []string{TransportRelays, TransportTailscale, TransportGateway} {
		if err := SaveTransport(l, tr); err != nil || LoadTransport(l).Transport != tr {
			t.Fatalf("%s: %v", tr, err)
		}
	}
	if r := relaysFor(TransportGateway); len(r) != len(relay.DefaultRelays)+1 || r[0] != relay.GatewayURL {
		t.Fatalf("gateway relays: %v", r)
	}
	if r := relaysFor(TransportTailscale); len(r) != 0 {
		t.Fatalf("tailscale relays: %v", r)
	}
}

// The panel's choice (core) is stored under the daemon's names: they must be the same words.
func TestPanelTransportNamesAreTheDaemonsNames(t *testing.T) {
	l := paths.Layout{Home: t.TempDir()}
	for _, name := range []string{core.TransportGateway, core.TransportRelays, core.TransportTailscale} {
		if err := SaveTransport(l, name); err != nil || LoadTransport(l).Transport != name {
			t.Errorf("%s: %v", name, err)
		}
	}
	if core.TransportGateway != TransportGateway || core.TransportRelays != TransportRelays || core.TransportTailscale != TransportTailscale {
		t.Fatal("the names differ")
	}
}

func fakeTailscale(t *testing.T, out string, err error) {
	t.Helper()
	old := runCmd
	runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "tailscale" || strings.Join(args, " ") != "status --json" {
			t.Errorf("ran %s %v", name, args)
		}
		return []byte(out), err
	}
	t.Cleanup(func() { runCmd = old })
}

func TestTailscaleIsReadyOnlyWhenRunningAndSignedIn(t *testing.T) {
	for name, tc := range map[string]struct {
		out  string
		err  error
		want bool
	}{
		"running":         {`{"BackendState":"Running","Self":{"DNSName":"mini.tail1.ts.net."}}`, nil, true},
		"signed out":      {`{"BackendState":"NeedsLogin","Self":{"DNSName":""}}`, nil, false},
		"stopped":         {`{"BackendState":"Stopped","Self":{"DNSName":"mini.tail1.ts.net."}}`, nil, false},
		"running no name": {`{"BackendState":"Running","Self":{"DNSName":"."}}`, nil, false},
		"not installed":   {``, errors.New("executable file not found"), false},
		"garbage":         {`not json`, nil, false},
	} {
		fakeTailscale(t, tc.out, tc.err)
		if got := tailscaleReady(); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// startFailingTailscale starts the panel transport with Tailscale stored and failing; switched says whether this
// process was started by the restart of a panel connection change.
func startFailingTailscale(t *testing.T, switched bool) *core.Core {
	t.Helper()
	rl := relaytest.New()
	t.Cleanup(rl.Close)
	t.Setenv("RUBI_RELAYS", rl.URL())
	t.Setenv("RUBI_PANEL_API_PORT", "0")
	t.Setenv("RUBI_PUBLIC_URL", "")
	l := paths.Layout{Home: t.TempDir()}
	if err := SaveTransport(l, TransportTailscale); err != nil {
		t.Fatal(err)
	}
	old := serveTailscale
	serveTailscale = func(context.Context, string) (string, error) {
		return "", errors.New("Tailscale isn't running or logged in")
	}
	t.Cleanup(func() { serveTailscale = old })
	c, err := core.Open(l)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Lock)
	if switched {
		c.MarkStartedByTransportSwitch()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := startPanelTransport(ctx, c, l); err != nil {
		t.Fatal(err)
	}
	return c
}

func failedEvent(c *core.Core) string {
	for _, e := range c.Events.List(true) {
		if e.Integration == "rubi" && e.Type == "transport.failed" {
			return fmt.Sprint(e.Data["message"])
		}
	}
	return ""
}

// Tailscale just switched to in the panel and failing at the first start: the user can't open the new link, so the
// panel stays reachable (Rubi Gateway's relays), the problem is recorded, and the Bot is told.
func TestTailscaleFailureRightAfterTheSwitchFallsBackAndTellsTheBot(t *testing.T) {
	c := startFailingTailscale(t, true)
	deadline := time.Now().Add(10 * time.Second)
	for c.RelaysConnected() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if c.RelaysConnected() == 0 {
		t.Fatal("no relay carries the panel after Tailscale failed right after the switch")
	}
	info := c.TransportInfo()
	if info["fallback"] != TransportGateway || info["transport"] != TransportTailscale {
		t.Fatalf("%v", info)
	}
	s := c.TransportSettings()
	if s["current"] != TransportTailscale || s["fallback"] != TransportGateway ||
		!strings.Contains(fmt.Sprint(s["notice"]), "Rubi Gateway or the public relays until Tailscale works") {
		t.Fatalf("%v", s)
	}
	if m := failedEvent(c); !strings.Contains(m, "isn't running") || !strings.Contains(m, "Rubi Gateway or the public relays until Tailscale works") {
		t.Fatalf("the Bot was told: %q", m)
	}
}

// Any other start (a reboot, a later outage) must not open public relays for a panel the user wanted only on their
// tailnet: no listener, the problem recorded, the Bot told.
func TestTailscaleFailureOnAPlainStartOpensNothingPublic(t *testing.T) {
	c := startFailingTailscale(t, false)
	time.Sleep(700 * time.Millisecond)
	if c.RelaysConnected() != 0 {
		t.Fatal("a relay carries the panel")
	}
	if info := c.TransportInfo(); info["fallback"] != nil || info["problem"] == nil {
		t.Fatalf("%v", info)
	}
	if s := c.TransportSettings(); s["fallback"] != nil || s["notice"] != nil {
		t.Fatalf("%v", s)
	}
	if m := failedEvent(c); !strings.Contains(m, "isn't running") || !strings.Contains(m, "closed to the public") {
		t.Fatalf("the Bot was told: %q", m)
	}
	if _, err := c.Link("settings"); err == nil {
		t.Fatal("a link was offered with no way to reach Rubi")
	}
}

// The reason a restart happens travels with the key to the next process, which reports it.
func TestHandoffReasonRoundTrip(t *testing.T) {
	for reason, want := range map[string]struct{ transport, update bool }{
		"":                 {false, true},
		"transport":        {true, false},
		"update":           {false, true},
		"transport,update": {true, true},
	} {
		l := paths.Layout{Home: t.TempDir()}
		c1, err := core.Open(l)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c1.Lock)
		if err := c1.Vault.Create(vault.NewKey(), vault.NewData("")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(l.Keys(), []byte("{}"), 0o600); err != nil { // so the next process finds it paired
			t.Fatal(err)
		}
		for _, r := range strings.Split(reason, ",") {
			if r != "" {
				c1.ArmRestartForTests("/bin/rubi", r)
			}
		}
		payload, err := handoffBytes(c1)
		if err != nil {
			t.Fatal(err)
		}
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(payload)
		w.Close()
		// receiveHandoff closes the descriptor it is given, so give it a copy and close ours now.
		fd, err := syscall.Dup(int(r.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
		t.Setenv("RUBI_HANDOFF_FD", strconv.Itoa(fd))
		c2, err := core.Open(l)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c2.Lock)
		if err := receiveHandoff(c2); err != nil {
			t.Fatalf("%q: %v", reason, err)
		}
		if c2.State() != core.Unlocked {
			t.Fatalf("%q: still locked", reason)
		}
		var tr, up bool
		for _, e := range c2.Events.List(true) {
			tr = tr || e.Type == "transport.changed"
			up = up || e.Type == "update.completed"
		}
		if c2.StartedByTransportSwitch() != want.transport {
			t.Errorf("%q: started by a switch = %v", reason, c2.StartedByTransportSwitch())
		}
		if tr != want.transport || up != want.update {
			t.Errorf("%q: transport.changed %v update.completed %v", reason, tr, up)
		}
	}
}
