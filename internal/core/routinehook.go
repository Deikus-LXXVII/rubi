package core

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
)

// A Grok Bot routine's webhook trigger: POST https://api2.cursor.sh/automations/webhook/<routine id> with
// Authorization: Bearer crsr_<key>. Other https receivers (a relay such as Hookdeck, a local test server)
// still work, but the approval says the address is not a Grok Bot routine.
const (
	routineHost       = "api2.cursor.sh"
	routinePathPrefix = "/automations/webhook/"
	routineKeyPrefix  = "crsr_"
	maxWebhookURL     = 2048
	maxWebhookKey     = 512
)

// isRoutineWebhook reports whether rawURL is a Grok Bot routine's webhook address.
func isRoutineWebhook(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	id := strings.TrimPrefix(u.EscapedPath(), routinePathPrefix)
	return strings.EqualFold(u.Host, routineHost) && id != u.EscapedPath() && id != "" && !strings.Contains(id, "/")
}

// cleanWebhookKey takes the routine's key as copied: the bare key, "Bearer <key>" or the whole
// "Authorization: Bearer <key>" header. The key goes into an HTTP header, so it must be one printable token.
func cleanWebhookKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if k, ok := strings.CutPrefix(strings.ToLower(key), "authorization:"); ok {
		key = strings.TrimSpace(key[len(key)-len(k):])
	}
	if strings.HasPrefix(strings.ToLower(key), "bearer ") {
		key = strings.TrimSpace(key[len("bearer "):])
	}
	if key == "" || strings.EqualFold(key, "bearer") {
		return "", errors.New("the webhook key is missing")
	}
	if len(key) > maxWebhookKey {
		return "", errors.New("the webhook key is too long; copy only the routine's key or header")
	}
	for _, r := range key {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return "", errors.New("the webhook key contains spaces or invalid characters; copy only the routine's key or header")
		}
	}
	return key, nil
}
