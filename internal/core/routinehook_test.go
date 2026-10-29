package core

import (
	"strings"
	"testing"
)

const testRoutineURL = "https://api2.cursor.sh/automations/webhook/6076a0ff-5b26-405e-82ef-d6173cd96066"

func TestIsRoutineWebhook(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{"routine", testRoutineURL, true},
		{"host in capitals", "https://API2.cursor.sh/automations/webhook/abc", true},
		{"http", "http://api2.cursor.sh/automations/webhook/abc", false},
		{"other host", "https://evil.example/automations/webhook/abc", false},
		{"look-alike suffix", "https://api2.cursor.sh.evil.example/automations/webhook/abc", false},
		{"look-alike prefix", "https://xapi2.cursor.sh/automations/webhook/abc", false},
		{"userinfo trick", "https://api2.cursor.sh@evil.example/automations/webhook/abc", false},
		{"user on the real host", "https://u:p@api2.cursor.sh/automations/webhook/abc", false},
		{"port", "https://api2.cursor.sh:8443/automations/webhook/abc", false},
		{"no routine id", "https://api2.cursor.sh/automations/webhook/", false},
		{"other path", "https://api2.cursor.sh/v0/me", false},
		{"deeper path", "https://api2.cursor.sh/automations/webhook/abc/../../v0/me", false},
		{"javascript", "javascript:alert(1)", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRoutineWebhook(tt.url); got != tt.want {
				t.Errorf("isRoutineWebhook(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestValidWebhookURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"routine", testRoutineURL, false},
		{"other https receiver", "https://events.hookdeck.example/e/abc", false},
		{"local test server", "http://127.0.0.1:8080/hook", false},
		{"http elsewhere", "http://api2.cursor.sh/automations/webhook/abc", true},
		{"javascript", "javascript:alert(1)", true},
		{"data", "data:text/html,hi", true},
		{"no host", "https:///automations/webhook/abc", true},
		{"userinfo trick", "https://api2.cursor.sh@evil.example/automations/webhook/abc", true},
		{"newline", testRoutineURL + "\r\nX-Evil: 1", true},
		{"too long", testRoutineURL + strings.Repeat("a", maxWebhookURL), true},
		{"empty", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validWebhookURL(tt.url); (err != nil) != tt.wantErr {
				t.Errorf("validWebhookURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestCleanWebhookKey(t *testing.T) {
	const key = "crsr_0123456789abcdef0123456789abcdef"
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"bare", key, key, false},
		{"bearer", "Bearer " + key, key, false},
		{"header", "Authorization: Bearer " + key, key, false},
		{"header in other case", "authorization:bearer   " + key, key, false},
		{"surrounding whitespace", "\n\t " + key + " \n", key, false},
		{"other receiver's key", "s3cret-token", "s3cret-token", false},
		{"empty", "", "", true},
		{"only bearer", "Bearer ", "", true},
		{"header injection", key + "\r\nX-Evil: 1", "", true},
		{"space inside", "crsr_abc def", "", true},
		{"two keys", key + " " + key, "", true},
		{"control character", "crsr_abc\x00def", "", true},
		{"non-ascii", "crsr_abcé", "", true},
		{"too long", "crsr_" + strings.Repeat("a", maxWebhookKey), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cleanWebhookKey(tt.in)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("cleanWebhookKey(%q) = %q, %v; want %q, error %v", tt.in, got, err, tt.want, tt.wantErr)
			}
		})
	}
}
