package home

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveSetupStatus probes this computer through a fresh Rubi Home directory and prints the setup page's
// status. Skipped unless RUBI_AGENT_LIVE=1; RUBI_AGENT_LIVE_PROJECT (optional) is added as an agent project.
// Status alone only reads; RUBI_AGENT_LIVE_RUN may run the service or image step (no privilege needed).
func TestLiveSetupStatus(t *testing.T) {
	if os.Getenv("RUBI_AGENT_LIVE") != "1" {
		t.Skip("set RUBI_AGENT_LIVE=1 to probe this computer")
	}
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if p := os.Getenv("RUBI_AGENT_LIVE_PROJECT"); p != "" {
		if err := h.Update(func(c *Config) { c.AgentProjects = []string{p} }); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	// RUBI_AGENT_LIVE_RUN names one unprivileged step to run first (service, image).
	if step := os.Getenv("RUBI_AGENT_LIVE_RUN"); step == "service" || step == "image" {
		if err := h.Setup.Run(ctx, step, "", "", ""); err != nil {
			t.Fatal(err)
		}
		for h.Setup.Busy() && ctx.Err() == nil {
			time.Sleep(2 * time.Second)
		}
	}
	// RUBI_AGENT_LIVE_RUN=login-url starts the shared sign-in, waits for its link and cancels: no code is sent.
	if os.Getenv("RUBI_AGENT_LIVE_RUN") == "login-url" {
		if err := h.Setup.Run(ctx, "login", "", "claudeai", ""); err != nil {
			t.Fatal(err)
		}
		url := ""
		for i := 0; i < 90 && url == "" && h.Setup.Busy(); i++ {
			time.Sleep(2 * time.Second)
			if l := h.Setup.Status(ctx).Login; l != nil {
				url = l.URL
			}
		}
		h.Setup.Cancel()
		for h.Setup.Busy() && ctx.Err() == nil {
			time.Sleep(time.Second)
		}
		if !strings.HasPrefix(url, "https://claude.com/") {
			t.Errorf("no sign-in link: %q", url)
		} else {
			t.Logf("sign-in link host ok (%d characters)", len(url))
		}
	}
	b, _ := json.MarshalIndent(h.Setup.Status(ctx), "", "  ")
	t.Logf("status:\n%s", b)
}
