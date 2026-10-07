package tunnel

import "testing"

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
