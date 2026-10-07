package core

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	_ "github.com/Deikus-LXXVII/rubi/internal/integrations/icloudmail"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

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
	if err := c.Pair(dek, keys, vault.NewData("")); err != nil {
		t.Fatal(err)
	}
	if c.State() != Unlocked || c.CheckPairingCode(f.Get("p")) {
		t.Fatal("after pairing: should be unlocked and the code burned")
	}
	if _, err := c.Link("pair"); err == nil {
		t.Fatal("pair link after pairing")
	}

	c.Lock()
	if c.State() != Locked {
		t.Fatalf("state %s", c.State())
	}
	if err := c.Unlock(vault.NewKey(), 0, nil); err == nil {
		t.Fatal("unlocked with a wrong key")
	}
	if err := c.Unlock(dek, 1, nil); err != nil || c.State() != Unlocked {
		t.Fatalf("unlock: %v %s", err, c.State())
	}

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
