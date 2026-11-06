package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/home"
)

const (
	bridgeA = "001788fffe123456"
	bridgeB = "001788fffe654321"
)

func pinBridges(t *testing.T, f *fakeHook) {
	t.Helper()
	if err := f.home.Update(func(c *home.Config) {
		c.HuePins = map[string]string{bridgeA: "aa", bridgeB: "bb"}
	}); err != nil {
		t.Fatal(err)
	}
}

func TestForgetHueBridgeNeedsAnApprovalAndForgetsOnlyThatBridge(t *testing.T) {
	c, _, _ := botCore(t)
	f := &fakeHook{}
	dev := pairedFakeHome(t, c, f)
	pinBridges(t, f)
	ctx := context.Background()

	id, err := c.ForgetHueBridge(ctx, dev, strings.ToUpper(bridgeA))
	if err != nil || id == "" {
		t.Fatalf("%q %v", id, err)
	}
	snap, err := c.Approvals.Get(id)
	if err != nil || snap.Kind != "rubi.settings" || snap.State != approvals.Pending || snap.Level != approvals.Strong {
		t.Fatalf("%+v %v", snap, err)
	}
	det, _ := c.Approvals.Detail(id)
	pv, _ := det.Preview.(map[string]any)
	if pv["bridge"] != bridgeA || !strings.Contains(snap.Summary, bridgeA) {
		t.Fatalf("the approval doesn't name the bridge: %q %v", snap.Summary, det.Preview)
	}
	time.Sleep(100 * time.Millisecond)
	if len(f.home.Config().HuePins) != 2 {
		t.Fatal("a pin went before the user approved")
	}
	snap, err = c.Approvals.DecidePanel(ctx, id, "apply", true, approvals.Verified)
	if err != nil || snap.State != approvals.Executed {
		t.Fatalf("%+v %v", snap, err)
	}
	pins := f.home.Config().HuePins
	if _, there := pins[bridgeA]; there || pins[bridgeB] != "bb" || len(pins) != 1 {
		t.Fatalf("pins after: %v", pins)
	}
	// The panel learns the bridges from the computer's hello.
	hello, err := c.CheckDevice(ctx, dev)
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := hello["hue_bridges"].([]any); len(list) != 1 || list[0] != bridgeB {
		t.Fatalf("hello.hue_bridges: %v", hello["hue_bridges"])
	}
}

func TestForgetHueBridgeRefusesWhatIsNotABridgeOrAComputer(t *testing.T) {
	c, _, _ := botCore(t)
	f := &fakeHook{}
	dev := pairedFakeHome(t, c, f)
	for _, bad := range []string{"", "xyz", bridgeA + "0", "../" + bridgeA[3:], bridgeA[:15] + "g"} {
		if id, err := c.ForgetHueBridge(context.Background(), dev, bad); err == nil || id != "" {
			t.Errorf("bridge %q accepted", bad)
		}
	}
	if _, err := c.ForgetHueBridge(context.Background(), "nope", bridgeA); err == nil {
		t.Error("an unknown computer was accepted")
	}
	if _, err := c.ForgetHueBridge(context.Background(), "", bridgeA); err == nil {
		t.Error("an empty computer reference (the first one) was accepted")
	}
}

// A plugin that may talk to Hue can't forget a pin; only the panel's approval can.
func TestAPluginCannotForgetAHueBridge(t *testing.T) {
	c, _, _ := botCore(t)
	f := &fakeHook{}
	dev := pairedFakeHome(t, c, f)
	pinBridges(t, f)
	_, err := c.homeCall(context.Background(), "hue", []string{"hue"}, dev, "hue.forget", json.RawMessage(`{"id":"`+bridgeA+`"}`))
	if err == nil || !strings.Contains(err.Error(), "Rubi refused") {
		t.Fatalf("%v", err)
	}
	if len(f.home.Config().HuePins) != 2 {
		t.Fatal("the plugin's call reached Home")
	}
}
