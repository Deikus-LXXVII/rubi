package tunnel

import (
	"errors"
	"strings"
	"testing"
)

func TestURLFromCloudflaredLog(t *testing.T) {
	lines := map[string]string{
		"2026-10-07T15:03:33Z INF |  https://quality-biography-gallery-schemes.trycloudflare.com                 |": "https://quality-biography-gallery-schemes.trycloudflare.com",
		"2026-10-07T15:03:33Z INF Requesting new quick Tunnel on trycloudflare.com...":                              "",
		"https://evil.example.com/https://x.trycloudflare.com.evil":                                                 "https://x.trycloudflare.com",
	}
	for line, want := range lines {
		if got := urlRe.FindString(line); got != want {
			t.Errorf("%q: got %q want %q", line, got, want)
		}
	}
}

func TestExitError(t *testing.T) {
	exit := errors.New("exit status 1")
	limited := exitError(exit, []string{
		"2026-10-08T04:48:08Z INF Requesting new quick Tunnel on trycloudflare.com...",
		"quick tunnel provisioning failed with status 429",
	})
	if !errors.Is(limited, ErrRateLimited) {
		t.Fatalf("429 not recognized: %v", limited)
	}
	other := exitError(exit, []string{"ERR failed to dial edge: connection refused"})
	if errors.Is(other, ErrRateLimited) || !strings.Contains(other.Error(), "connection refused") {
		t.Fatalf("other failure: %v", other)
	}
}
