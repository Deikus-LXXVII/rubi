package core

import (
	"errors"
	"fmt"
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
	minBotWebhookKey  = 20 // a Grok routine key is crsr_ plus about 86 characters
)

// keyFileHint is how a Bot should hand over its key instead.
const keyFileHint = "Write it with `umask 077; printenv RUBI_WEBHOOK_KEY_<NAME> > /workspace/.rubi-hook-key` and pass " +
	"key_file \"/workspace/.rubi-hook-key\" instead of key (Rubi reads and deletes the file), or omit both and let the user paste it"

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

// keyPlaceholder refuses a key a tool replaced with a redaction placeholder or a mask (raw, as received).
func keyPlaceholder(raw string) error {
	t := strings.TrimSpace(raw)
	if strings.Contains(strings.ToUpper(t), "REDACTED") || (t != "" && strings.Trim(t, "*•") == "") {
		return errors.New("the webhook key arrived as a redaction placeholder, not the key: your tool replaced the secret before sending it.")
	}
	return nil
}

// keyTooShort refuses a key that was cut on the way. It never repeats the key.
func keyTooShort(key string) error {
	if len(key) < minBotWebhookKey {
		return fmt.Errorf("the webhook key is only %d characters; a routine key is much longer, so it was cut or replaced on the way.", len(key))
	}
	return nil
}

// keyRoutineFormat requires the routine key format crsr_[A-Za-z0-9_-] (as the panel's paste parser,
// panel/js/webhook.js). It never repeats the key.
func keyRoutineFormat(key string) error {
	rest, ok := strings.CutPrefix(key, routineKeyPrefix)
	if !ok || strings.TrimLeft(rest, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") != "" {
		return errors.New("this isn't a Grok Bot routine key (" + routineKeyPrefix + "followed by letters, digits, _ or -); " +
			"copy the routine's key or its Authorization header.")
	}
	return nil
}

// cleanRoutineKey cleans a key pasted for webhook url and, for a Grok Bot routine address, holds it to
// the routine key rules (no placeholder, long enough, crsr_ format), the same ones botWebhookKey applies.
func cleanRoutineKey(raw, url string) (string, error) {
	routine := isRoutineWebhook(url)
	if routine {
		if err := keyPlaceholder(raw); err != nil {
			return "", err
		}
	}
	key, err := cleanWebhookKey(raw)
	if err != nil {
		return "", err
	}
	if routine {
		if err := keyTooShort(key); err != nil {
			return "", err
		}
		if err := keyRoutineFormat(key); err != nil {
			return "", err
		}
	}
	return key, nil
}

// botWebhookKey checks a key a Bot proposes. A Bot's key passed through its host, which may have replaced
// it (a redaction placeholder) or cut it, so this is stricter than what the user pastes: no placeholder,
// at least minBotWebhookKey characters for any address, and for a Grok Bot routine address the routine key
// format. Errors say how a Bot hands the key over instead.
func botWebhookKey(raw, url string) (string, error) {
	if err := keyPlaceholder(raw); err != nil {
		return "", errors.New(err.Error() + " " + keyFileHint)
	}
	key, err := cleanWebhookKey(raw)
	if err != nil {
		return "", errors.New(err.Error() + ". " + keyFileHint)
	}
	if err := keyTooShort(key); err != nil {
		return "", errors.New(err.Error() + " " + keyFileHint)
	}
	if isRoutineWebhook(url) {
		if err := keyRoutineFormat(key); err != nil {
			return "", errors.New(err.Error() + " " + keyFileHint)
		}
	}
	return key, nil
}
