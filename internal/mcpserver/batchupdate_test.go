package mcpserver_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/version"
)

// Project rule 5 (docs/project-rules.md), updates: Rubi's own update and the plugins' updates are one approval,
// "Update everything", with Rubi as one item and each plugin as another.

// releaseProbe stands in for Rubi's installing step (a test must not replace its own binary) and for the daemon's
// stop function, and notes what the plugins were at the moment Rubi was installed.
type releaseProbe struct {
	mu        sync.Mutex
	calls     int
	atInstall map[string]string // plugin -> version when Rubi's install ran
	err       error
	stops     atomic.Int32
}

func (p *releaseProbe) callCount() int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

// updateRig has alpha and beta installed at v1.0.0 and v1.1.0 of both in the store, and Rubi v99.0.0 released.
func updateRig(t *testing.T, rubiRelease string) (*rig, *releaseProbe) {
	t.Helper()
	old := version.Version
	version.Version = "v0.1.0-test" // a test build: the release feed and checksums may come from the test registry
	t.Cleanup(func() { version.Version = old })
	r, reg := storeRig(t)
	approveItems(r, r.ag.call("rubi_plugin_install", map[string]any{"plugins": []string{"alpha", "beta"}}), "install_quiet")
	for _, id := range []string{"alpha", "beta"} {
		reg.ReviewAs(id, "v1.1.0", reg.PublishAs(id, "v1.1.0", nil, nil), "")
	}
	reg.PublishRubi(rubiRelease)
	p := &releaseProbe{}
	r.c.InstallRelease = func(_ context.Context, _ string) (string, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.calls++
		p.atInstall = map[string]string{"alpha": versionOf(r, "alpha"), "beta": versionOf(r, "beta")}
		return "/bin/rubi", p.err
	}
	r.c.SetShutdown(func() { p.stops.Add(1) })
	return r, p
}

func TestUpdateEverythingIsOneApproval(t *testing.T) {
	r, p := updateRig(t, "v99.0.0")
	out := r.ag.call("rubi_update", map[string]any{"everything": true})
	if out["approval_url"] == nil || out["tool_error"] != nil {
		t.Fatalf("update everything: %v", out)
	}
	for _, kind := range []string{"rubi.update", "rubi.plugin.update"} {
		if n := waitingKind(r, kind); n != 0 {
			t.Fatalf("%d %s approvals wait besides the combined one", n, kind)
		}
	}
	if n := waitingKind(r, "rubi.update.all"); n != 1 {
		t.Fatalf("%d combined approvals wait; want 1", n)
	}
	items := itemPreviews(t, r, out)
	if len(items) != 3 || items["rubi"]["new_version"] != "v99.0.0" || items["plugin:alpha"]["new_version"] != "v1.1.0" ||
		items["plugin:beta"]["current"] != "v1.0.0" {
		t.Fatalf("items: %v", items)
	}
	// Asking again gives the same link, not a second approval.
	if again := r.ag.call("rubi_update", map[string]any{"everything": true}); again["approval_id"] != out["approval_id"] || waitingKind(r, "rubi.update.all") != 1 {
		t.Fatalf("a second ask: %v", again)
	}

	res := approveItems(r, out, "update")
	if len(res["updated"].([]any)) != 2 || res["restarting"] != true || res["rubi_failed"] != nil || res["failed"] != nil {
		t.Fatalf("result: %v", res)
	}
	if versionOf(r, "alpha") != "v1.1.0" || versionOf(r, "beta") != "v1.1.0" {
		t.Fatalf("plugins after: %s %s", versionOf(r, "alpha"), versionOf(r, "beta"))
	}
	// The order: the plugins were already updated when Rubi was installed and the restart armed.
	if p.callCount() != 1 || p.atInstall["alpha"] != "v1.1.0" || p.atInstall["beta"] != "v1.1.0" {
		t.Fatalf("Rubi installed %d times; plugins at that moment %v", p.callCount(), p.atInstall)
	}
	// One restart is armed, for the update, and the daemon is stopped once.
	if got := r.c.HandoffReason(); got != "update" {
		t.Fatalf("restart reason %q", got)
	}
	r.waitFor("the one stop", func() bool { return p.stops.Load() >= 1 })
	time.Sleep(300 * time.Millisecond)
	if n := p.stops.Load(); n != 1 {
		t.Fatalf("stopped %d times", n)
	}
}

func TestUpdateEverythingWithoutRubi(t *testing.T) {
	r, p := updateRig(t, "v99.0.0")
	out := r.ag.call("rubi_update", map[string]any{"everything": true})
	res := approveItems(r, out, "items:plugin:alpha,plugin:beta") // Rubi unticked
	if len(res["updated"].([]any)) != 2 || res["rubi"] != nil || res["restarting"] != nil {
		t.Fatalf("result: %v", res)
	}
	if versionOf(r, "alpha") != "v1.1.0" || versionOf(r, "beta") != "v1.1.0" {
		t.Fatalf("plugins after: %s %s", versionOf(r, "alpha"), versionOf(r, "beta"))
	}
	if p.callCount() != 0 || r.c.HandoffReason() != "" {
		t.Fatalf("Rubi was updated though unticked: %d, restart %q", p.callCount(), r.c.HandoffReason())
	}
	time.Sleep(1700 * time.Millisecond)
	if n := p.stops.Load(); n != 0 {
		t.Fatalf("stopped %d times with no Rubi update", n)
	}
}

func TestUpdateEverythingWithoutPlugins(t *testing.T) {
	r, p := updateRig(t, "v99.0.0")
	out := r.ag.call("rubi_update", map[string]any{"everything": true})
	res := approveItems(r, out, "items:rubi") // both plugins unticked
	if u, _ := res["updated"].([]any); res["restarting"] != true || len(u) != 0 {
		t.Fatalf("result: %v", res)
	}
	if versionOf(r, "alpha") != "v1.0.0" || versionOf(r, "beta") != "v1.0.0" {
		t.Fatalf("plugins changed though unticked: %s %s", versionOf(r, "alpha"), versionOf(r, "beta"))
	}
	if p.callCount() != 1 || r.c.HandoffReason() != "update" {
		t.Fatalf("Rubi: %d installs, restart %q", p.callCount(), r.c.HandoffReason())
	}
}

func TestUpdateEverythingKeepsPluginsWhenRubiFails(t *testing.T) {
	r, p := updateRig(t, "v99.0.0")
	p.err = errors.New("download failed")
	out := r.ag.call("rubi_update", map[string]any{"everything": true})
	res := approveItems(r, out, "update")
	if res["rubi_failed"] == nil || len(res["updated"].([]any)) != 2 || res["restarting"] != nil {
		t.Fatalf("result: %v", res)
	}
	if versionOf(r, "alpha") != "v1.1.0" {
		t.Fatalf("the plugin update was lost with Rubi's failure: %s", versionOf(r, "alpha"))
	}
	if r.c.HandoffReason() != "" {
		t.Fatalf("a restart was armed though Rubi was not installed: %q", r.c.HandoffReason())
	}
}

func TestUpdateEverythingWithOnlyOneKindOfUpdate(t *testing.T) {
	// Only Rubi has an update: the ordinary Rubi approval.
	r, _ := updateRig(t, "v99.0.0")
	approveItems(r, r.ag.call("rubi_plugin_update", map[string]any{"all": true}), "install")
	out := r.ag.call("rubi_update", map[string]any{"everything": true})
	info, err := r.panelLink(out["approval_url"].(string)).ApprovalInfo(out["approval_id"].(string))
	if err != nil || info.Approval.Kind != "rubi.update" || len(info.Approval.Items) != 0 {
		t.Fatalf("Rubi only: %+v %v", info.Approval, err)
	}

	// Only plugins have updates: the ordinary plugin update.
	r2, _ := updateRig(t, "v0.1.0-test") // Rubi is current
	out = r2.ag.call("rubi_update", map[string]any{"everything": true})
	info, err = r2.panelLink(out["approval_url"].(string)).ApprovalInfo(out["approval_id"].(string))
	if err != nil || info.Approval.Kind != "rubi.plugin.update" || len(info.Approval.Items) != 2 {
		t.Fatalf("plugins only: %+v %v", info.Approval, err)
	}
	if out["rubi"] != "up_to_date" {
		t.Fatalf("the answer does not say Rubi is current: %v", out)
	}
	// And plain rubi_update is unchanged.
	if out := r2.ag.call("rubi_update", nil); out["status"] != "up_to_date" {
		t.Fatalf("rubi_update: %v", out)
	}
}
