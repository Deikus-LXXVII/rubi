package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// A Rubi Home that says "not available on this computer" reaches the plugin as its own error code, with Home's sentence, so a
// plugin (the T3 Agents setup page, Apple Home) can tell the user and stop, without matching text. Any other failure stays a
// plain failure.
func TestPluginGetsNotAvailableHereAsItsOwnCode(t *testing.T) {
	c, _, _ := botCore(t)
	sentence := "That isn't available on this computer (agents don't run on Windows yet)."
	f := &fakeHook{fail: map[string]error{
		"t3-setup.status": &homeproto.CodedError{Code: homeproto.CodeNotAvailableHere, Message: sentence},
		"t3-setup.cancel": errors.New("something else broke"),
	}}
	pairedFakeHome(t, c, f)
	_, err := c.homeCall(context.Background(), t3Plugin, []string{"t3-setup"}, "", "t3-setup.status", json.RawMessage(`{}`))
	var pe *rubiplugin.Error
	if !errors.As(err, &pe) || pe.Code != rubiplugin.CodeHomeNotAvailable || !strings.HasSuffix(pe.Message, sentence) {
		t.Fatalf("%v", err)
	}
	_, err = c.homeCall(context.Background(), t3Plugin, []string{"t3-setup"}, "", "t3-setup.cancel", json.RawMessage(`{}`))
	if !errors.As(err, &pe) || pe.Code != rubiplugin.CodeHomeFailed {
		t.Fatalf("a plain failure: %v", err)
	}
}

// The approval for pairing names the shortcuts only for a computer that has them; the Mac text is what it always was.
func TestPairingApprovalNamesShortcutsOnlyWhereTheyExist(t *testing.T) {
	mac := allowsText("Rubi", true)
	if mac != "control Philips Hue on your home network, run the shortcuts in the folder \"Rubi\", answer Claude Code's permission requests there and pass messages to and from its Claude Code sessions (only plugins you approve for that can)" {
		t.Fatalf("the Mac text changed: %s", mac)
	}
	if other := allowsText("Rubi", false); strings.Contains(other, "shortcuts") || !strings.HasPrefix(other, "control Philips Hue on your home network, answer Claude") {
		t.Fatalf("%s", other)
	}
}

// The panel's check of a computer carries Home's features through, so it can say what the computer can't do.
func TestCheckDeviceCarriesTheFeatures(t *testing.T) {
	c, _, _ := botCore(t)
	dev := pairedFakeHome(t, c, &fakeHook{})
	res, err := c.CheckDevice(context.Background(), dev)
	if err != nil {
		t.Fatal(err)
	}
	feats, ok := res["features"].(map[string]any)
	if !ok {
		t.Fatalf("no features in %v", res)
	}
	for _, name := range []string{"shortcuts", "apple_home", "claude-start", "agent", "t3"} {
		f, ok := feats[name].(map[string]any)
		if !ok {
			t.Fatalf("no %s in %v", name, feats)
		}
		if _, ok := f["available"].(bool); !ok {
			t.Errorf("%s: %v", name, f)
		}
	}
}
