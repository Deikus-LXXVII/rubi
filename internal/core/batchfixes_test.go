package core

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/plugins/plugintest"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

// Tests for the guards behind the batch approvals (project rule 5): what is applied must still be what the user
// was asked, and downloads that wait for an approval must stay until it ends.

// marketCore is an unlocked Rubi with a Bot connected and a test registry (reviewed plugins alpha, beta and gamma
// at v1.0.0 once published) as the store. The build counts as a test build, so the registry's release feed counts.
func marketCore(t *testing.T) (*Core, *plugintest.Registry) {
	t.Helper()
	old := version.Version
	version.Version = "v0.1.0-test"
	t.Cleanup(func() { version.Version = old })
	reg := plugintest.New(t)
	c, _, _ := botCore(t)
	_ = c.Vault.Update(func(d *vault.Data) error {
		d.Agents = append(d.Agents, &vault.Agent{Name: "Main", URL: "https://hook.example/x", Key: "k", Default: true})
		return nil
	})
	return c, reg
}

func publishReviewed(reg *plugintest.Registry, id, ver string) {
	reg.ReviewAs(id, ver, reg.PublishAs(id, ver, nil, nil), "")
}

func decideOK(t *testing.T, c *Core, res map[string]any, option string) approvals.Snapshot {
	t.Helper()
	id, _ := res["approval_id"].(string)
	if id == "" {
		t.Fatalf("no approval: %v", res)
	}
	snap, err := c.Approvals.DecidePanel(context.Background(), id, option, true, approvals.Verified)
	if err != nil {
		t.Fatalf("decide %s: %v", option, err)
	}
	return snap
}

// installNow installs plugins (ids or sources) through the real approval, ticking every one.
func installNow(t *testing.T, c *Core, refs ...string) {
	t.Helper()
	res, err := c.RequestPluginInstalls(context.Background(), refs)
	if err != nil {
		t.Fatal(err)
	}
	option := "install_quiet"
	if len(refs) > 1 {
		ids, _ := res["plugins"].([]string)
		option += "|items:" + strings.Join(ids, ",")
	}
	if snap := decideOK(t, c, res, option); snap.State != approvals.Executed {
		t.Fatalf("install: %+v", snap)
	}
}

func recordOf(t *testing.T, c *Core, id string) vault.Plugin {
	t.Helper()
	r := c.record(id)
	if r == nil {
		t.Fatalf("%s is not installed", id)
	}
	return *r
}

func stagingDirs(c *Core) int {
	entries, _ := os.ReadDir(c.Store.Staging())
	return len(entries)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// M1: an approval for an older step is refused at commit, whatever route brought the plugin forward.
func TestStaleOrRepeatedPluginUpdateIsRefused(t *testing.T) {
	c, reg := marketCore(t)
	ctx := context.Background()
	publishReviewed(reg, "alpha", "v1.0.0")
	installNow(t, c, "alpha")
	publishReviewed(reg, "alpha", "v1.1.0")
	first, _, err := c.preparePluginUpdate(ctx, "alpha")
	if err != nil || first == nil {
		t.Fatal(err)
	}
	second, _, err := c.preparePluginUpdate(ctx, "alpha") // another approval for the same step
	if err != nil || second == nil {
		t.Fatal(err)
	}
	if err := c.applyPluginUpdate(first); err != nil {
		t.Fatal(err)
	}
	// the same version applied twice
	if err := c.applyPluginUpdate(second); err == nil || !strings.Contains(err.Error(), "changed since you were asked; nothing was done") {
		t.Fatalf("a second apply of 1.1.0: %v", err)
	}
	// 1.2.0 goes in; the approval for 1.0.0 -> 1.1.0 is now stale and must not put 1.1.0 over it
	publishReviewed(reg, "alpha", "v1.2.0")
	third, _, err := c.preparePluginUpdate(ctx, "alpha")
	if err != nil || third == nil {
		t.Fatal(err)
	}
	if err := c.applyPluginUpdate(third); err != nil {
		t.Fatal(err)
	}
	if err := c.applyPluginUpdate(second); err == nil {
		t.Fatal("a stale update was applied over a newer release")
	}
	rec := recordOf(t, c, "alpha")
	if rec.Version != "v1.2.0" || rec.Previous == nil || rec.Previous.Version != "v1.1.0" {
		t.Fatalf("record %s, rollback target %+v: the way back must stay 1.1.0", rec.Version, rec.Previous)
	}
}

// M1b: two installs of one plugin waited; the second must not install over the first.
func TestSecondInstallOfTheSamePluginIsRefused(t *testing.T) {
	c, reg := marketCore(t)
	ctx := context.Background()
	publishReviewed(reg, "alpha", "v1.0.0")
	one, err := c.RequestPluginInstall(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	two, err := c.RequestPluginInstall(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if snap := decideOK(t, c, one, "install_quiet"); snap.State != approvals.Executed {
		t.Fatalf("first install: %+v", snap)
	}
	_ = c.Vault.Update(func(d *vault.Data) error { // the user connected it meanwhile
		d.Integrations["alpha"] = &vault.Integration{Enabled: true, Accounts: []*vault.Account{{ID: "ann", Label: "ann", Default: true}}}
		return nil
	})
	before := recordOf(t, c, "alpha")
	snap := decideOK(t, c, two, "install_quiet")
	if snap.State != approvals.Failed || !strings.Contains(snap.Error, "already installed") {
		t.Fatalf("second install: %+v", snap)
	}
	after := recordOf(t, c, "alpha")
	var accounts int
	_ = c.Vault.View(func(d *vault.Data) error { accounts = len(d.Integrations["alpha"].Accounts); return nil })
	if !after.InstalledAt.Equal(before.InstalledAt) || after.Tree != before.Tree || accounts != 1 {
		t.Fatalf("the installed plugin or its account was touched: %+v -> %+v, accounts %d", before, after, accounts)
	}
}

// M1 combined: a plugin update waiting and the combined approval share one slot.
func TestPluginUpdateAndUpdateEverythingShareOneSlot(t *testing.T) {
	c, reg := marketCore(t)
	ctx := context.Background()
	publishReviewed(reg, "alpha", "v1.0.0")
	publishReviewed(reg, "beta", "v1.0.0")
	installNow(t, c, "alpha", "beta")
	publishReviewed(reg, "alpha", "v1.1.0")
	publishReviewed(reg, "beta", "v1.1.0")
	reg.PublishRubi("v99.0.0")
	c.InstallRelease = func(context.Context, string) (string, error) { return "/bin/rubi", nil }

	all, err := c.RequestUpdateAll(ctx)
	if err != nil || all["approval_id"] == nil {
		t.Fatalf("update everything: %v %v", all, err)
	}
	plugin, err := c.RequestPluginUpdates(ctx, nil)
	if err != nil || plugin["approval_id"] == nil {
		t.Fatalf("plugin update: %v %v", plugin, err)
	}
	if s, _ := c.Approvals.Get(all["approval_id"].(string)); s.State != approvals.Cancelled {
		t.Fatalf("the combined approval still waits beside a plugin update: %+v", s)
	}
	// and the reverse
	again, err := c.RequestUpdateAll(ctx)
	if err != nil || again["approval_id"] == nil {
		t.Fatal(err)
	}
	if s, _ := c.Approvals.Get(plugin["approval_id"].(string)); s.State != approvals.Cancelled {
		t.Fatalf("the plugin update still waits beside the combined approval: %+v", s)
	}
	if s, _ := c.Approvals.Get(again["approval_id"].(string)); s.State != approvals.Pending {
		t.Fatalf("%+v", s)
	}
}

// M1 combined, stale: the combined approval waits, a plugin is reinstalled meanwhile; that item is refused and
// the others still apply.
func TestStaleItemOfUpdateEverythingIsRefusedAndTheOthersApply(t *testing.T) {
	c, reg := marketCore(t)
	ctx := context.Background()
	publishReviewed(reg, "alpha", "v1.0.0")
	publishReviewed(reg, "beta", "v1.0.0")
	installNow(t, c, "alpha", "beta")
	publishReviewed(reg, "alpha", "v1.1.0")
	publishReviewed(reg, "beta", "v1.1.0")
	reg.PublishRubi("v99.0.0")
	c.InstallRelease = func(context.Context, string) (string, error) { return "/bin/rubi", nil }
	all, err := c.RequestUpdateAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// alpha goes to 1.1.0 by another route (removed, installed again from the store)
	rm, _ := c.RequestPluginRemove(ctx, "alpha")
	decideOK(t, c, rm, "remove")
	installNow(t, c, "alpha")
	snap := decideOK(t, c, all, "update")
	out, _ := snap.Result.(map[string]any)
	failed, _ := out["failed"].(map[string]string)
	if snap.State != approvals.Executed || !strings.Contains(failed["alpha"], "changed since you were asked") || failed["beta"] != "" {
		t.Fatalf("result: %+v", snap)
	}
	if recordOf(t, c, "beta").Version != "v1.1.0" || recordOf(t, c, "alpha").Version != "v1.1.0" {
		t.Fatal("beta did not update, or alpha moved")
	}
}

// m1: two Rubi approvals; only one may install.
func TestOnlyOneRubiUpdateInstalls(t *testing.T) {
	c, _ := marketCore(t)
	var calls atomic.Int32
	c.InstallRelease = func(context.Context, string) (string, error) { calls.Add(1); return "/bin/rubi", nil }
	if _, err := c.applyUpdate(context.Background(), "v99.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.applyUpdate(context.Background(), "v99.0.0"); err == nil {
		t.Fatal("the second approval installed again")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("installRelease ran %d times; a second run would put the new binary into .prev", n)
	}
	if c.HandoffReason() != "update" {
		t.Fatalf("reason %q", c.HandoffReason())
	}
}

func TestUpdateToAnOlderOrSameVersionIsRefused(t *testing.T) {
	c, _ := marketCore(t)
	c.InstallRelease = func(context.Context, string) (string, error) { return "", errors.New("must not run") }
	for _, v := range []string{"v0.1.0-test", "v0.0.9"} {
		if _, err := c.applyUpdate(context.Background(), v); err == nil || strings.Contains(err.Error(), "must not run") {
			t.Fatalf("%s: %v", v, err)
		}
	}
}

func TestATransportRestartDoesNotBlockAnUpdate(t *testing.T) {
	c, _ := marketCore(t)
	var calls atomic.Int32
	c.InstallRelease = func(context.Context, string) (string, error) { calls.Add(1); return "/bin/rubi", nil }
	c.armRestart("/bin/rubi", restartTransport)
	if _, err := c.applyUpdate(context.Background(), "v99.0.0"); err != nil || calls.Load() != 1 {
		t.Fatalf("%v, %d installs", err, calls.Load())
	}
	if c.HandoffReason() != "transport,update" {
		t.Fatalf("reason %q", c.HandoffReason())
	}
}

// m2: more plugins than one approval holds.
func TestUpdateEverythingSaysHowManyPluginsWereLeftOut(t *testing.T) {
	c, reg := marketCore(t)
	ctx := context.Background()
	for _, id := range []string{"alpha", "beta", "gamma"} {
		publishReviewed(reg, id, "v1.0.0")
	}
	installNow(t, c, "alpha", "beta", "gamma")
	for _, id := range []string{"alpha", "beta", "gamma"} {
		publishReviewed(reg, id, "v1.1.0")
	}
	reg.PublishRubi("v99.0.0")
	c.InstallRelease = func(context.Context, string) (string, error) { return "/bin/rubi", nil }
	old := maxBatch
	maxBatch = 2
	t.Cleanup(func() { maxBatch = old })

	res, err := c.RequestUpdateAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	det, _ := c.Approvals.Detail(res["approval_id"].(string))
	pv, _ := det.Preview.(map[string]any)
	if len(det.Items) != 3 || pv["left_out"] == nil || !strings.Contains(pv["left_out"].(string), "1 more plugin") || res["left_out"] != 1 {
		t.Fatalf("items %d, preview %v, answer %v", len(det.Items), pv, res)
	}
	snap := decideOK(t, c, res, "update")
	out, _ := snap.Result.(map[string]any)
	if out["left_out"] != 1 || recordOf(t, c, "gamma").Version != "v1.0.0" {
		t.Fatalf("result %v", out)
	}
}

// The per-item warning of a plugin from outside the store survives in "update everything".
func TestUpdateEverythingKeepsTheUnreviewedWarning(t *testing.T) {
	c, reg := marketCore(t)
	publishReviewed(reg, "alpha", "v1.0.0")
	reg.PublishAs("gamma", "v1.0.0", nil, nil) // not in the store
	installNow(t, c, "alpha", reg.SourceOf("gamma"))
	publishReviewed(reg, "alpha", "v1.1.0")
	reg.PublishAs("gamma", "v1.1.0", nil, nil)
	reg.PublishRubi("v99.0.0")
	c.InstallRelease = func(context.Context, string) (string, error) { return "/bin/rubi", nil }
	res, err := c.RequestUpdateAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	det, _ := c.Approvals.Detail(res["approval_id"].(string))
	var g, a map[string]any
	for _, it := range det.Items {
		switch it.Key {
		case "plugin:gamma":
			g, _ = it.Preview.(map[string]any)
		case "plugin:alpha":
			a, _ = it.Preview.(map[string]any)
		}
	}
	if g["warning"] == nil || g["review"] != "Not reviewed by Rubi-Project" || a["warning"] != nil || a["review"] != "Reviewed by Rubi-Project" {
		t.Fatalf("gamma %v alpha %v", g, a)
	}
}

// m4: two requests whose downloads overlap; neither may lose its files to the other's cleaning.
func TestOverlappingDownloadsKeepTheirFiles(t *testing.T) {
	c, reg := marketCore(t)
	publishReviewed(reg, "alpha", "v1.0.0")
	publishReviewed(reg, "beta", "v1.0.0")
	reg.SlowDownloads(400 * time.Millisecond)
	var wg sync.WaitGroup
	results := make([]map[string]any, 2)
	errs := make([]error, 2)
	for i, id := range []string{"alpha", "beta"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = c.RequestPluginInstall(context.Background(), id)
		}()
		time.Sleep(100 * time.Millisecond) // the second starts while the first still downloads
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
	}
	if n := stagingDirs(c); n != 2 {
		t.Fatalf("%d staged downloads; want both", n)
	}
	for i, res := range results {
		if snap := decideOK(t, c, res, "install_quiet"); snap.State != approvals.Executed {
			t.Fatalf("install %d: %+v", i, snap)
		}
	}
}

// A refused, cancelled or expired batch leaves nothing in the staging folder.
func TestEndedBatchLeavesNoStagedFiles(t *testing.T) {
	for _, how := range []string{"refused", "cancelled", "expired"} {
		t.Run(how, func(t *testing.T) {
			c, reg := marketCore(t)
			publishReviewed(reg, "alpha", "v1.0.0")
			publishReviewed(reg, "beta", "v1.0.0")
			res, err := c.RequestPluginInstalls(context.Background(), []string{"alpha", "beta"})
			if err != nil {
				t.Fatal(err)
			}
			id := res["approval_id"].(string)
			if stagingDirs(c) != 2 {
				t.Fatalf("%d staged while waiting", stagingDirs(c))
			}
			switch how {
			case "refused":
				_, err = c.Approvals.DecidePanel(context.Background(), id, "", false, approvals.Verified)
			case "cancelled":
				_, err = c.Approvals.Cancel(id)
			case "expired":
				c.Approvals.SetClock(func() time.Time { return time.Now().Add(16 * time.Minute) })
				_, err = c.Approvals.Get(id)
			}
			if err != nil {
				t.Fatal(err)
			}
			waitUntil(t, "the staging folder to be emptied", func() bool { return stagingDirs(c) == 0 })
			if n := len(c.mkt.staged); n != 0 {
				t.Fatalf("%d staged entries remain in the bookkeeping", n)
			}
		})
	}
}

// m5: a staged download is kept at least as long as its approval can wait (15 minutes in the engine), and the
// cleaning does not stop on a clock of its own while the approval is still pending or being executed.
func TestStagingLivesAsLongAsTheApproval(t *testing.T) {
	c, reg := marketCore(t)
	publishReviewed(reg, "alpha", "v1.0.0")
	res, err := c.RequestPluginInstall(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := c.Approvals.Get(res["approval_id"].(string))
	if d := stagingDeadline(snap); !d.After(snap.ExpiresAt) || d.Sub(snap.ExpiresAt) < 30*time.Minute {
		t.Fatalf("a download is dropped at %v, expiry %v", d, snap.ExpiresAt)
	}
	// still there well after the old fixed 20 minutes, as long as the approval is not over
	time.Sleep(100 * time.Millisecond)
	if stagingDirs(c) != 1 {
		t.Fatal("the download went before the approval ended")
	}
}
