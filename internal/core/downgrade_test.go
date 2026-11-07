package core

import (
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

func TestDevBuildsAreNotTheHighestSeenVersion(t *testing.T) {
	prev := version.Version
	t.Cleanup(func() { version.Version = prev })

	c, err := Open(paths.Layout{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	c.SetEndpoint("http://127.0.0.1:1")
	dek := vault.NewKey()
	keys := &vault.Keys{Wraps: []vault.Wrap{{ID: "w1", Kind: "password", Nonce: "n", Wrapped: "x"}}}
	if err := c.Pair(dek, keys, vault.NewData(""), "password", ""); err != nil {
		t.Fatal(err)
	}

	unlock := func(v string) {
		t.Helper()
		version.Version = v
		c.Lock()
		if err := c.Unlock(dek, 0, "password", ""); err != nil {
			t.Fatalf("unlock %s: %v", v, err)
		}
	}
	highest := func() string {
		t.Helper()
		var got string
		_ = c.Vault.View(func(d *vault.Data) error {
			got = d.HighestVersion
			return nil
		})
		return got
	}

	for _, v := range []string{"dev", "v0.15.0-dev", "v0.14.1-5-gabcdef", "v9.9.9-test", "v1.0.0-dirty"} {
		unlock(v)
		if c.DowngradedFrom() != "" {
			t.Errorf("%s reported a downgrade from %s", v, c.DowngradedFrom())
		}
	}
	if highest() != "" {
		t.Fatalf("a dev build was recorded as highest: %s", highest())
	}

	// A dev build recorded by an older Rubi must not alarm when a release is unlocked, and must not stay the mark.
	if err := c.Vault.Update(func(d *vault.Data) error {
		d.HighestVersion = "v0.15.0-dev"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	unlock("v0.14.1")
	if c.DowngradedFrom() != "" {
		t.Fatalf("release after a dev build looked downgraded from %s", c.DowngradedFrom())
	}
	if highest() != "v0.14.1" {
		t.Fatalf("highest = %s, want v0.14.1", highest())
	}
	// A published pre-release still counts. It is not a local dev build.
	unlock("v0.14.2-rc.1")
	if c.DowngradedFrom() != "" || highest() != "v0.14.2-rc.1" {
		t.Fatalf("rc: from %q highest %s", c.DowngradedFrom(), highest())
	}

	// A real newer release still counts.
	unlock("v0.15.0")
	if highest() != "v0.15.0" || c.DowngradedFrom() != "" {
		t.Fatalf("forward: highest %s from %s", highest(), c.DowngradedFrom())
	}
	unlock("v0.14.1")
	if c.DowngradedFrom() != "v0.15.0" {
		t.Fatalf("a real rollback was not reported: %q", c.DowngradedFrom())
	}
}
