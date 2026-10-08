package mcpserver_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/core"
	"github.com/Deikus-LXXVII/rubi/internal/panelapi"
	"github.com/Deikus-LXXVII/rubi/internal/panelclient"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/relay/relaytest"
)

// TestPanelOverRelays: with no tunnel at all, the panel pairs, unlocks and reads settings through relays.
func TestPanelOverRelays(t *testing.T) {
	r1, r2 := relaytest.New(), relaytest.New()
	defer r1.Close()
	defer r2.Close()
	r1.Drop = true
	relays := []string{r1.URL(), r2.URL()}

	c, err := core.Open(paths.Layout{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Lock()
	if _, err := c.Link("pair"); err == nil {
		t.Fatal("link without any transport")
	}
	key, _ := relay.NewKey()
	c.SetRelay(key.Public(), relays)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&relay.Server{Key: key, Relays: relays, Handle: panelapi.New(c).HandleRPC, Ready: c.SetRelaysConnected}).Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for c.RelaysConnected() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	link, err := c.Link("pair")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(link, "e=") || !strings.Contains(link, "n="+key.Public()) || !strings.Contains(link, "r=") {
		t.Fatalf("link: %s", link)
	}
	client := func(purpose string) *panelclient.Client {
		l, _ := c.Link(purpose)
		pl, err := panelclient.ParseLink(l)
		if err != nil {
			t.Fatal(err)
		}
		pc, err := panelclient.New(pl)
		if err != nil {
			t.Fatal(err)
		}
		return pc
	}
	if _, err := client("pair").PairWithPassword(pw); err != nil {
		t.Fatal(err)
	}
	c.Lock()
	if _, err := client("unlock").UnlockWithPassword(pw, 0); err != nil || c.State() != core.Unlocked {
		t.Fatalf("unlock over relays: %v %s", err, c.State())
	}
	var st map[string]any
	if err := client("settings").Call("status", nil, &st); err != nil || st["state"] != "unlocked" {
		t.Fatalf("status over relays: %v %v", st, err)
	}
}
