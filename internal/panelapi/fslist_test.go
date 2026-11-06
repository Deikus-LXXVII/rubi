package panelapi_test

import (
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/panelclient"
)

// The folder browser shows the names of the user's folders: only a full settings link may ask for them. A link
// that was forwarded (a view-only settings link, an approval, a setup or an agent link) gets the "open Settings"
// answer before anything is looked up; a settings link gets as far as the computer lookup.
func TestFolderListingIsForFullSettingsLinksOnly(t *testing.T) {
	c, _ := setup(t)
	if _, err := client(t, c, "pair").PairWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	sc := client(t, c, "settings")
	view := client(t, c, "approve:apr_x")
	var vs struct{ Ticket string }
	if err := view.Call("session.settings", nil, &vs); err != nil {
		t.Fatal(err)
	}
	viewOnly := *view
	viewOnly.Link.Ticket = vs.Ticket
	var out map[string]any
	for name, pc := range map[string]*panelclient.Client{"view-only": &viewOnly, "approval": view, "agent": client(t, c, "agent:Main")} {
		err := pc.Call("device.fs.list", map[string]any{"id": "d1", "path": "/"}, &out)
		if err == nil || !strings.Contains(err.Error(), "settings link") && !strings.Contains(err.Error(), "can't change settings") && !strings.Contains(err.Error(), "only sets up") && !strings.Contains(err.Error(), "only connects") {
			t.Errorf("%s link: %v", name, err)
		}
	}
	err := sc.Call("device.fs.list", map[string]any{"id": "nope", "path": ""}, &out)
	if err == nil || strings.Contains(err.Error(), "settings link") {
		t.Fatalf("a settings link was stopped at the gate: %v", err)
	}
	if !strings.Contains(err.Error(), "unknown") && !strings.Contains(err.Error(), "Rubi Home") {
		t.Errorf("a settings link: %v", err)
	}
}
