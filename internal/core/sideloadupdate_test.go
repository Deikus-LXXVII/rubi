package core

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/plugins/plugintest"
	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// In "update everything" a plugin from outside the store is ticked by default only when its release is signed by
// the key pinned at install and asks for no new permissions. (A changed key is refused outright when the release is
// fetched, so for the user the case that remains is the new permissions.)

// sideRig has alpha (from the store) and gamma (sideloaded) installed at v1.0.0, alpha v1.1.0 in the store and a
// new Rubi release; gamma's release is up to the test.
func sideRig(t *testing.T) (*Core, *plugintest.Registry) {
	t.Helper()
	c, reg := marketCore(t)
	publishReviewed(reg, "alpha", "v1.0.0")
	reg.PublishAs("gamma", "v1.0.0", nil, nil)
	installNow(t, c, "alpha", reg.SourceOf("gamma"))
	publishReviewed(reg, "alpha", "v1.1.0")
	reg.PublishRubi("v99.0.0")
	c.InstallRelease = func(context.Context, string) (string, error) { return "/bin/rubi", nil }
	return c, reg
}

func itemOf(t *testing.T, c *Core, res map[string]any, key string) approvals.Item {
	t.Helper()
	det, err := c.Approvals.Detail(res["approval_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range det.Items {
		if it.Key == key {
			return it
		}
	}
	t.Fatalf("no item %s in %+v", key, det.Items)
	return approvals.Item{}
}

func TestSideloadedUpdateWithTheSameKeyAndPermissionsIsTicked(t *testing.T) {
	c, reg := sideRig(t)
	reg.PublishAs("gamma", "v1.1.0", nil, nil)
	res, err := c.RequestUpdateAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	g := itemOf(t, c, res, "plugin:gamma")
	if g.DefaultOff || itemOf(t, c, res, "plugin:alpha").DefaultOff {
		t.Fatalf("an update with the same key and no new permissions starts unticked: %+v", g)
	}
	// the plain approval takes both
	snap := decideOK(t, c, res, "update")
	if recordOf(t, c, "gamma").Version != "v1.1.0" || snap.State != approvals.Executed {
		t.Fatalf("%+v", snap)
	}
}

func TestSideloadedUpdateWithNewPermissionsStartsUnticked(t *testing.T) {
	c, reg := sideRig(t)
	reg.PublishAs("gamma", "v1.1.0", nil, func(m map[string]any) { m["egress"] = []string{"example.com:443", "api.example.net:443"} })
	res, err := c.RequestUpdateAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	g := itemOf(t, c, res, "plugin:gamma")
	p, _ := g.Preview.(map[string]any)
	why, _ := p["starts_unticked"].(string)
	if !g.DefaultOff || !strings.Contains(why, "new permissions") || !strings.Contains(why, "api.example.net:443") ||
		p["new_permissions"] == nil || p["warning"] == nil || !strings.Contains(g.Label, "not ticked") {
		t.Fatalf("gamma: off %v label %q preview %v", g.DefaultOff, g.Label, p)
	}
	if itemOf(t, c, res, "plugin:alpha").DefaultOff || itemOf(t, c, res, "rubi").DefaultOff {
		t.Fatal("the reviewed update or Rubi's started unticked")
	}
	// the plain approval leaves gamma alone; naming it installs it
	snap := decideOK(t, c, res, "update")
	if recordOf(t, c, "gamma").Version != "v1.0.0" || recordOf(t, c, "alpha").Version != "v1.1.0" || snap.State != approvals.Executed {
		t.Fatalf("gamma %s alpha %s", recordOf(t, c, "gamma").Version, recordOf(t, c, "alpha").Version)
	}
	res2, err := c.RequestUpdateAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	decideOK(t, c, res2, "items:plugin:gamma")
	if recordOf(t, c, "gamma").Version != "v1.1.0" {
		t.Fatal("ticking gamma on purpose did not update it")
	}
}

// A release signed by another key is refused when it is fetched, as before: it never reaches the card.
func TestSideloadedUpdateWithAnotherKeyIsStillRefused(t *testing.T) {
	c, reg := sideRig(t)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	reg.PublishAs("gamma", "v1.1.0", other, nil)
	res, err := c.RequestUpdateAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	failed, _ := res["failed"].(map[string]string)
	if !strings.Contains(failed["gamma"], "different publisher key") {
		t.Fatalf("failed: %v", res["failed"])
	}
	det, _ := c.Approvals.Detail(res["approval_id"].(string))
	for _, it := range det.Items {
		if it.Key == "plugin:gamma" {
			t.Fatal("an update signed by another key is on the card")
		}
	}
}

// The rule itself, on manifests: any change to what confers power starts unticked, with its reason. This includes the
// key case that the fetch makes unreachable today (defence in depth).
func TestStartsUntickedRule(t *testing.T) {
	base := func() plugins.Manifest {
		m := plugins.Manifest{ID: "gamma", Name: "Gamma", Version: "v1.0.0", Entry: "gamma", Egress: []string{"example.com:443"},
			Actions: []rubiplugin.Action{{Kind: "gamma.send", Title: "Send", DefaultLevel: rubiplugin.Strong}},
			Tools:   []rubiplugin.Tool{{Name: "gamma_echo"}},
			Config:  []rubiplugin.ConfigField{{Key: "strict", Label: "Strict", Type: "bool", Default: true}},
			Home:    []string{"shortcuts"}}
		m.Publisher.Key = "K"
		return m
	}
	next := func(change func(m *plugins.Manifest)) plugins.Manifest {
		m := base()
		m.Version = "v1.1.0"
		m.Description = "A better description."
		if change != nil {
			change(&m)
		}
		return m
	}
	sideloaded := func(m plugins.Manifest) pluginUpdate {
		return pluginUpdate{old: base(), cand: &plugins.Candidate{Manifest: m}, pinnedKey: "K"}
	}
	for _, tc := range []struct {
		name   string
		u      pluginUpdate
		off    bool
		reason string // what the reason must contain
	}{
		{"store plugin, even with a new capability", pluginUpdate{old: base(), pinnedKey: "K",
			cand: &plugins.Candidate{Reviewed: true, Manifest: next(func(m *plugins.Manifest) { m.Home = append(m.Home, "claude-start") })}}, false, ""},
		{"only the version and description changed", sideloaded(next(nil)), false, ""},
		{"Home claude-start", sideloaded(next(func(m *plugins.Manifest) { m.Home = append(m.Home, "claude-start") })), true, "changed: home"},
		{"hooks", sideloaded(next(func(m *plugins.Manifest) { m.Hooks = true })), true, "private web addresses"},
		{"a new tool", sideloaded(next(func(m *plugins.Manifest) { m.Tools = append(m.Tools, rubiplugin.Tool{Name: "gamma_wipe"}) })), true, "new tool gamma_wipe"},
		{"a lowered action level", sideloaded(next(func(m *plugins.Manifest) { m.Actions[0].DefaultLevel = rubiplugin.None })), true, "now defaults to"},
		{"a changed config field", sideloaded(next(func(m *plugins.Manifest) { m.Config[0].Default = false })), true, "changed: config"},
		{"a new host", sideloaded(next(func(m *plugins.Manifest) { m.Egress = append(m.Egress, "evil.example:443") })), true, "connects to evil.example:443"},
		{"a panel page", sideloaded(next(func(m *plugins.Manifest) { m.Panel = "view" })), true, "changed: panel"},
		{"another executable", sideloaded(next(func(m *plugins.Manifest) { m.Entry = "other" })), true, "changed: entry"},
		{"a host dropped (any change counts)", sideloaded(next(func(m *plugins.Manifest) { m.Egress = nil })), true, "changed: egress"},
		{"a new secret", sideloaded(next(func(m *plugins.Manifest) { m.Secrets = []rubiplugin.Secret{{Key: "t", Label: "Token"}} })), true, "asks for Token"},
		{"a new event", sideloaded(next(func(m *plugins.Manifest) { m.Events = []rubiplugin.EventType{{Type: "ping"}} })), true, "can notify about ping"},
		{"a new setup field", sideloaded(next(func(m *plugins.Manifest) { m.Fields = []rubiplugin.Field{{Key: "u", Label: "User", Type: "text"}} })), true, "changed: fields"},
		{"the key changed", pluginUpdate{old: base(), cand: &plugins.Candidate{Manifest: func() plugins.Manifest { m := next(nil); m.Publisher.Key = "L"; return m }()}, pinnedKey: "K"}, true, "publisher key changed"},
		{"no key recorded", pluginUpdate{old: base(), cand: &plugins.Candidate{Manifest: next(nil)}}, true, "publisher key changed"},
	} {
		off, why := tc.u.startsUnticked()
		if off != tc.off || !strings.Contains(why, tc.reason) || (off && why == "") {
			t.Errorf("%s: off %v, reason %q (want %v, containing %q)", tc.name, off, why, tc.off, tc.reason)
		}
	}
}

// Through the whole path: a sideloaded update that adds a Rubi Home capability starts unticked, with the reason.
func TestSideloadedUpdateThatAddsAHomeCapabilityStartsUnticked(t *testing.T) {
	c, reg := sideRig(t)
	reg.PublishAs("gamma", "v1.1.0", nil, func(m map[string]any) { m["home"] = []string{"shortcuts", "claude-start"} })
	res, err := c.RequestUpdateAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	g := itemOf(t, c, res, "plugin:gamma")
	p, _ := g.Preview.(map[string]any)
	if why, _ := p["starts_unticked"].(string); !g.DefaultOff || !strings.Contains(why, "Rubi Home") || p["new_permissions"] == nil {
		t.Fatalf("off %v, preview %v", g.DefaultOff, p)
	}
}

// The plain update approval lists these too: Home capabilities, hooks, new tools and a lowered approval level.
func TestNewPermissionsListsHomeHooksToolsAndALoweredLevel(t *testing.T) {
	old := plugins.Manifest{Home: []string{"shortcuts"}, Tools: []rubiplugin.Tool{{Name: "g_a"}},
		Actions: []rubiplugin.Action{{Kind: "g.send", Title: "Send", DefaultLevel: rubiplugin.Strong}}}
	m := plugins.Manifest{Home: []string{"shortcuts", "claude-start"}, Hooks: true, Tools: []rubiplugin.Tool{{Name: "g_a"}, {Name: "g_b"}},
		Actions: []rubiplugin.Action{{Kind: "g.send", Title: "Send", DefaultLevel: rubiplugin.Tap}}}
	got := newPermissions(old, m)
	for _, want := range []string{"uses Rubi Home for", "private web addresses", "new tool g_b", "send now defaults to Passkey tap"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is not in %q", want, got)
		}
	}
	if strings.Contains(got, "g_a") {
		t.Errorf("an old tool is listed as new: %q", got)
	}
	if newPermissions(old, old) != "" {
		t.Error("nothing changed, yet something is listed")
	}
}
