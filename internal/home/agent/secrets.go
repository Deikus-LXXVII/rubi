package agent

import (
	"bytes"
	"regexp"
	"strings"
)

var secretMarkers = []string{"sk-ant", "bearer", "token", "code=", "refresh"}

// secretWord is a word long enough, in the characters of base64, base64url and dotted tokens, to be a key,
// a token or a sign-in code.
var secretWord = regexp.MustCompile(`[A-Za-z0-9._#~+/=-]{32,}`)

// LooksSecret says whether a line of output might carry a secret (a key, a bearer token, a sign-in code, a
// refresh token); such lines are dropped from logs and progress lists.
func LooksSecret(line string) bool {
	low := strings.ToLower(line)
	for _, m := range secretMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return secretWord.MatchString(line)
}

var credentialRE = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`)

// RedactCredentials replaces anything shaped like an Anthropic credential with "[redacted credential]". It is
// the second line of defense behind the runner's own redaction, applied to what Home hands to the plugin.
func RedactCredentials(b []byte) []byte {
	if !bytes.Contains(b, []byte("sk-ant-")) {
		return b
	}
	return credentialRE.ReplaceAll(b, []byte("[redacted credential]"))
}
