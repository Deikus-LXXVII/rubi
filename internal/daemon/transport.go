package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// How the user's browser reaches the panel API. The user picks one when Rubi is installed (the agent asks):
//
//	gateway   Rubi Gateway, a relay run for Rubi only, with the public relays as a fallback (default)
//	relays    public Nostr relays only
//	tailscale the user's own Tailscale network (tailscale serve); the panel opens only on their devices
const (
	TransportGateway   = "gateway"
	TransportRelays    = "relays"
	TransportTailscale = "tailscale"
)

// tailscalePort is the fixed local port the panel API uses with Tailscale (tailscale serve points at it).
const tailscalePort = "47391"

// TransportConfig is stored in plaintext (Rubi needs it before it is unlocked; it holds no secret).
type TransportConfig struct {
	Transport string `json:"transport"`
}

func configPath(l paths.Layout) string { return l.Home + "/transport.json" }

// LoadTransport reads the chosen transport (gateway if none was chosen).
func LoadTransport(l paths.Layout) TransportConfig {
	cfg := TransportConfig{Transport: TransportGateway}
	if b, err := os.ReadFile(configPath(l)); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	switch cfg.Transport {
	case TransportGateway, TransportRelays, TransportTailscale:
	default:
		cfg.Transport = TransportGateway
	}
	return cfg
}

// SaveTransport stores the user's choice.
func SaveTransport(l paths.Layout, transport string) error {
	switch transport {
	case TransportGateway, TransportRelays, TransportTailscale:
	default:
		return fmt.Errorf("transport must be %q, %q or %q", TransportGateway, TransportRelays, TransportTailscale)
	}
	if err := l.Ensure(); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(TransportConfig{Transport: transport}, "", "  ")
	return vault.WriteFileAtomic(configPath(l), b, 0o600)
}

// relaysFor returns the relays a transport uses (none for Tailscale).
func relaysFor(transport string) []string {
	if r := os.Getenv("RUBI_RELAYS"); r != "" {
		return strings.Split(r, ",")
	}
	switch transport {
	case TransportGateway:
		return append([]string{relay.GatewayURL}, relay.DefaultRelays...)
	case TransportRelays:
		return relay.DefaultRelays
	}
	return nil
}

// setupTailscale serves the panel API on this machine's tailnet name and returns the URL. It needs
// Tailscale installed and logged in; with an operator-less tailscaled it falls back to sudo -n.
func setupTailscale(ctx context.Context, port string) (string, error) {
	out, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
	if err != nil {
		return "", errors.New("Tailscale isn't running or logged in on this machine (tailscale status failed)")
	}
	var st struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if json.Unmarshal(out, &st) != nil || st.Self.DNSName == "" {
		return "", errors.New("couldn't read this machine's Tailscale name")
	}
	name := strings.TrimSuffix(st.Self.DNSName, ".")
	args := []string{"serve", "--bg", "--https=8443", "http://127.0.0.1:" + port}
	if out, err := exec.CommandContext(ctx, "tailscale", args...).CombinedOutput(); err != nil {
		if out2, err2 := exec.CommandContext(ctx, "sudo", append([]string{"-n", "tailscale"}, args...)...).CombinedOutput(); err2 != nil {
			return "", fmt.Errorf("tailscale serve failed: %s %s", strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
		}
	}
	return "https://" + name + ":8443", nil
}
