package daemon

import (
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
)

func TestTransportChoice(t *testing.T) {
	l := paths.Layout{Home: t.TempDir()}
	if got := LoadTransport(l).Transport; got != TransportGateway {
		t.Fatalf("default: %s", got)
	}
	if err := SaveTransport(l, "carrier-pigeon"); err == nil {
		t.Fatal("unknown transport saved")
	}
	for _, tr := range []string{TransportRelays, TransportTailscale, TransportGateway} {
		if err := SaveTransport(l, tr); err != nil || LoadTransport(l).Transport != tr {
			t.Fatalf("%s: %v", tr, err)
		}
	}
	if r := relaysFor(TransportGateway); len(r) != len(relay.DefaultRelays)+1 || r[0] != relay.GatewayURL {
		t.Fatalf("gateway relays: %v", r)
	}
	if r := relaysFor(TransportTailscale); len(r) != 0 {
		t.Fatalf("tailscale relays: %v", r)
	}
}
