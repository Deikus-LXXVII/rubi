package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// installManifest puts a plugin manifest in place without the package (enough for policy lookups).
func installManifest(t *testing.T, c *Core, raw string) {
	t.Helper()
	var m plugins.Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if err := plugins.Check(&m); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "stage")
	_ = os.MkdirAll(dir, 0o700)
	tree, _ := plugins.TreeHash(dir)
	if err := c.Store.Commit(&plugins.Candidate{Manifest: m, Dir: dir, Tree: tree}, ""); err != nil {
		t.Fatal(err)
	}
}

func fragment(t *testing.T, link string) url.Values {
	t.Helper()
	i := strings.Index(link, "#")
	if i < 0 || !strings.HasPrefix(link, DefaultPanelOrigin+"/#") {
		t.Fatalf("bad link %q", link)
	}
	v, err := url.ParseQuery(link[i+1:])
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestLifecycle(t *testing.T) {
	c, err := Open(paths.Layout{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if c.State() != Unpaired {
		t.Fatalf("state %s", c.State())
	}
	if _, err := c.Link("pair"); !errors.Is(err, ErrNoTransport) {
		t.Fatalf("link without transport: %v", err)
	}
	c.SetEndpoint("https://example.trycloudflare.com")
	if _, err := c.Link("unlock"); err == nil {
		t.Fatal("unlock link before pairing")
	}

	link, err := c.Link("pair")
	if err != nil {
		t.Fatal(err)
	}
	f := fragment(t, link)
	if f.Get("e") != "https://example.trycloudflare.com" || f.Get("k") != c.ID.PublicBundle() || f.Get("p") == "" {
		t.Fatalf("pair fragment %v", f)
	}
	again, _ := c.Link("pair")
	if fragment(t, again).Get("p") != f.Get("p") {
		t.Fatal("pairing code changed within its lifetime")
	}
	if c.CheckPairingCode("wrong") || !c.CheckPairingCode(f.Get("p")) {
		t.Fatal("pairing code check")
	}

	dek := vault.NewKey()
	keys := &vault.Keys{Wraps: []vault.Wrap{{ID: "w1", Kind: "password", Nonce: "n", Wrapped: "x", CreatedAt: time.Now()}}}
	if err := c.Pair(dek, keys, vault.NewData(""), "password", ""); err != nil {
		t.Fatal(err)
	}
	if c.State() != Unlocked || c.CheckPairingCode(f.Get("p")) {
		t.Fatal("after pairing: should be unlocked and the code burned")
	}
	if _, err := c.Link("pair"); err == nil {
		t.Fatal("pair link after pairing")
	}
	ul, err := c.Link("unlock")
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := c.TicketPurpose(fragment(t, ul).Get("t")); !ok || p != "unlock" {
		t.Fatalf("ticket: %q %v", p, ok)
	}
	if _, ok := c.TicketPurpose("forged"); ok {
		t.Fatal("forged ticket accepted")
	}

	c.Lock()
	if c.State() != Locked {
		t.Fatalf("state %s", c.State())
	}
	if err := c.Unlock(vault.NewKey(), 0, "password", ""); err == nil {
		t.Fatal("unlocked with a wrong key")
	}
	if err := c.Unlock(dek, 1, "password", ""); err != nil || c.State() != Unlocked {
		t.Fatalf("unlock: %v %s", err, c.State())
	}
	_ = c.Vault.View(func(d *vault.Data) error {
		if len(d.Receipts) != 2 || d.Receipts[0].Event != "paired" || d.Receipts[1].Event != "unlocked" {
			t.Fatalf("receipts: %+v", d.Receipts)
		}
		return nil
	})

	// A reopened core (daemon restart) starts locked.
	c2, err := Open(c.Layout)
	if err != nil {
		t.Fatal(err)
	}
	if c2.State() != Locked {
		t.Fatalf("after restart: %s", c2.State())
	}
}

func TestPolicyLevel(t *testing.T) {
	c, _ := Open(paths.Layout{Home: t.TempDir()})
	installManifest(t, c, `{"schema":1,"id":"icloud-mail","name":"iCloud Mail","version":"v1.0.0","api":1,
		"entry":"icloud-mail","publisher":{"name":"x","key":"MCowBQYDK2VwAyEAxeDfKAkO77JdARN7Y2jJT3tXw9mN+GqqH8R5mhcxt8c="},
		"actions":[{"kind":"icloud-mail.read","title":"Read","default_level":"none"},
		           {"kind":"icloud-mail.send","title":"Send","default_level":"strong"}]}`)
	if got := c.PolicyLevel("rubi.plugin.install"); got != approvals.Strong {
		t.Fatalf("core kinds must be strong, got %s", got)
	}
	if got := c.PolicyLevel("icloud-mail.send"); got != approvals.Strong {
		t.Fatalf("default send level %s", got)
	}
	if got := c.PolicyLevel("unknown.kind"); got != approvals.Strong {
		t.Fatalf("unknown kinds must be strong, got %s", got)
	}
	_ = c.Vault.Create(vault.NewKey(), vault.NewData(""))
	_ = c.Vault.Update(func(d *vault.Data) error {
		d.Policy["icloud-mail.send"] = "chat"
		d.Policy["icloud-mail.read"] = "bogus"
		return nil
	})
	if got := c.PolicyLevel("icloud-mail.send"); got != approvals.Chat {
		t.Fatalf("user policy ignored: %s", got)
	}
	if got := c.PolicyLevel("icloud-mail.read"); got != approvals.None {
		t.Fatalf("invalid policy value should fall back to the default: %s", got)
	}
}

// The tap level (a passkey tap without Face ID) can be chosen for ordinary plugin actions, never for a
// locked action or a core (rubi.*) kind, even if the vault says so.
func TestTapLevelCantLowerLockedOrCoreKinds(t *testing.T) {
	c, _ := Open(paths.Layout{Home: t.TempDir()})
	installManifest(t, c, `{"schema":1,"id":"claude-code","name":"Claude Code","version":"v0.1.0","api":1,
		"entry":"claude-code","publisher":{"name":"x","key":"MCowBQYDK2VwAyEAxeDfKAkO77JdARN7Y2jJT3tXw9mN+GqqH8R5mhcxt8c="},
		"actions":[{"kind":"claude-code.run","title":"Run","default_level":"strong","locked":true},
		           {"kind":"claude-code.run_safe","title":"Run safe","default_level":"tap"},
		           {"kind":"claude-code.other","title":"Other","default_level":"strong"}]}`)
	if got := c.PolicyLevel("claude-code.run_safe"); got != approvals.Tap {
		t.Fatalf("tap default: %s", got)
	}
	_ = c.Vault.Create(vault.NewKey(), vault.NewData(""))
	_ = c.Vault.Update(func(d *vault.Data) error {
		d.Policy["claude-code.run"] = "tap"
		d.Policy["rubi.plugin.install"] = "tap"
		d.Policy["rubi.secrets.set"] = "tap"
		d.Policy["claude-code.other"] = "tap"
		return nil
	})
	for _, k := range []string{"claude-code.run", "rubi.plugin.install", "rubi.secrets.set", "unknown.kind"} {
		if got := c.PolicyLevel(k); got != approvals.Strong {
			t.Errorf("%s lowered to %s", k, got)
		}
	}
	if got := c.PolicyLevel("claude-code.other"); got != approvals.Tap {
		t.Errorf("user policy tap ignored: %s", got)
	}
	ctx := context.Background()
	for _, k := range []string{"claude-code.run", "rubi.plugin.install", "rubi.settings", "unknown.kind"} {
		if _, err := c.SetPolicy(ctx, map[string]string{k: "tap"}); err == nil || !strings.Contains(err.Error(), "can't be changed") {
			t.Errorf("SetPolicy %s=tap: %v", k, err)
		}
	}
	if _, err := c.SetPolicy(ctx, map[string]string{"claude-code.other": "tap"}); err != nil && strings.Contains(err.Error(), "can't be changed") {
		t.Errorf("SetPolicy on an ordinary action: %v", err)
	}
}

func TestPanelOriginFixedAfterPairing(t *testing.T) {
	home := t.TempDir()
	layout := paths.Layout{Home: home}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	// Unpaired: the environment decides (self-hosted panels are set up this way).
	t.Setenv("RUBI_PANEL_ORIGIN", "https://panel.example")
	if o := panelOriginFor(layout, false, "v1.0.0"); o != "https://panel.example" {
		t.Fatalf("unpaired: %q", o)
	}
	// Paired with the default: a later change of the environment is ignored in release builds.
	_ = os.WriteFile(layout.PanelOrigin(), []byte(DefaultPanelOrigin+"\n"), 0o600)
	t.Setenv("RUBI_PANEL_ORIGIN", "https://rubi-panel.co")
	if o := panelOriginFor(layout, true, "v1.0.0"); o != DefaultPanelOrigin {
		t.Fatalf("paired: %q", o)
	}
	// Paired before origins were recorded: the default, not the environment.
	_ = os.Remove(layout.PanelOrigin())
	if o := panelOriginFor(layout, true, "v1.0.0"); o != DefaultPanelOrigin {
		t.Fatalf("paired, no record: %q", o)
	}
	// Test builds may always override it.
	if o := panelOriginFor(layout, true, "dev"); o != "https://rubi-panel.co" {
		t.Fatalf("dev: %q", o)
	}
}
