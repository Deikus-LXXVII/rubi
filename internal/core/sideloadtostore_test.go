package core

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/plugins/plugintest"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// deadSource is a sideload source nothing serves any more (the local server it was installed from is gone).
const deadSource = "http://127.0.0.1:1/gamma"

// sideloadedGamma installs gamma v1.0.0-dev.2 from outside the store, signed by key (nil: the registry's publisher), gives it
// an account and state, and points its source at a server that is gone.
func sideloadedGamma(t *testing.T, key ed25519.PrivateKey) (*Core, *plugintest.Registry) {
	t.Helper()
	c, reg := marketCore(t)
	reg.PublishAs("gamma", "v1.0.0-dev.2", key, nil)
	installNow(t, c, reg.SourceOf("gamma"))
	if r := recordOf(t, c, "gamma"); r.Reviewed || r.Version != "v1.0.0-dev.2" {
		t.Fatalf("installed %+v", r)
	}
	_ = c.Vault.Update(func(d *vault.Data) error {
		d.Plugins["gamma"].Source = deadSource
		d.Integrations["gamma"] = &vault.Integration{Enabled: true, State: json.RawMessage(`{"seen":42}`),
			Accounts: []*vault.Account{{ID: "me@example.com", Label: "me@example.com", Default: true}}}
		return nil
	})
	return c, reg
}

// A plugin sideloaded once and now in the store from the same publisher (the catalog's key is the key pinned at install)
// is updated from the store, even though its old source is gone: the approval says the source changes, and afterwards
// it is a store plugin (reviewed, the store's source) with its account and state kept and the sideloaded version as
// its rollback.
func TestSideloadedPluginMovesToTheStoreOnUpdate(t *testing.T) {
	c, reg := sideloadedGamma(t, nil)
	publishReviewed(reg, "gamma", "v1.1.0")
	res, err := c.RequestPluginUpdate(context.Background(), "gamma")
	if err != nil {
		t.Fatal(err)
	}
	// the background check (on the catalog the update request just fetched) sees it too, not the dead source's error
	if ok, err := c.updateAvailable(context.Background(), "gamma"); !ok || err != nil {
		t.Fatalf("update available: %v %v", ok, err)
	}
	det, err := c.Approvals.Detail(res["approval_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := det.Preview.(map[string]any)
	if why, _ := p["source_change"].(string); !strings.Contains(why, "Rubi store") || !strings.Contains(why, deadSource) {
		t.Fatalf("the approval doesn't say the source changes: %v", p)
	}
	if snap := decideOK(t, c, res, "install"); snap.State != approvals.Executed {
		t.Fatalf("%+v", snap)
	}
	r := recordOf(t, c, "gamma")
	if r.Version != "v1.1.0" || !r.Reviewed || r.Source != plugins.NormalizeSource(reg.SourceOf("gamma")) || r.PublisherKey != reg.PubKey {
		t.Fatalf("after the update: %+v", r)
	}
	if r.Previous == nil || r.Previous.Version != "v1.0.0-dev.2" || r.Previous.Source != deadSource {
		t.Fatalf("rollback target %+v", r.Previous)
	}
	_ = c.Vault.View(func(d *vault.Data) error {
		in := d.Integrations["gamma"]
		if in == nil || !in.Enabled || len(in.Accounts) != 1 || in.Accounts[0].ID != "me@example.com" || string(in.State) != `{"seen":42}` {
			t.Errorf("the plugin's account or state was not kept: %+v", in)
		}
		return nil
	})
}

// The store listing the same id with another publisher's key is another plugin: the sideloaded one stays on its
// source, and nothing changes.
func TestSideloadedPluginStaysOffTheStoreForAnotherPublisher(t *testing.T) {
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	c, reg := sideloadedGamma(t, other)
	publishReviewed(reg, "gamma", "v1.1.0") // signed by the registry's publisher, not other
	if _, err := c.RequestPluginUpdate(context.Background(), "gamma"); err == nil {
		t.Fatal("an update was offered from the store for another publisher's plugin")
	}
	r := recordOf(t, c, "gamma")
	if r.Reviewed || r.Version != "v1.0.0-dev.2" || r.Source != deadSource {
		t.Fatalf("the plugin changed: %+v", r)
	}
}

// The update check that tells the Bots finds the store's release for such a plugin (not the dead source's error) and
// notes it, with the store's release notes.
func TestUpdateCheckFindsTheStoreReleaseOfASideloadedPlugin(t *testing.T) {
	c, reg := sideloadedGamma(t, nil)
	publishReviewed(reg, "gamma", "v1.1.0")
	_ = c.Vault.Update(func(d *vault.Data) error { d.QuietUpdates = nil; return nil }) // its updates are announced
	c.CheckPluginUpdates(context.Background())
	if r := recordOf(t, c, "gamma"); r.Notified != "v1.1.0" {
		t.Fatalf("the check noted %q, want the store's v1.1.0", r.Notified)
	}
}
