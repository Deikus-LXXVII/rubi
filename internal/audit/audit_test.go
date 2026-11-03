package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secretShaped are values of the shapes Rubi handles: a routine key, a bearer header, an app password, a pairing
// code, a one-time ticket, a Home pairing code.
var secretShaped = []string{
	"crsr_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	"Authorization: Bearer s3cret-token-0123456789",
	"abcd-efgh-ijkl-mnop",
	"rubi-home:pX3k9QaLmmmmmmmmmmmm",
	"TICKET-9f8e7d6c5b4a39281716",
}

// Project rule 3: the audit log never holds a value in plain text. While Rubi is locked the detail fields are dropped;
// while unlocked they are sealed. Callers are asked not to pass secrets at all (TestNoSecretIsHandedToALogOrTheAudit in
// internal/repocheck reads the callers), so this is the second line.
func TestRecordNeverWritesAValueInPlainText(t *testing.T) {
	for _, unlocked := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "audit.log")
		l := Open(path)
		if unlocked {
			l.SetVaultKey([]byte("0123456789abcdef0123456789abcdef"))
		}
		f := Fields{}
		for i, s := range secretShaped {
			f[string(rune('a'+i))] = s
		}
		l.Record("secret.test", f)
		l.Record("secret.test", Fields{"nested": map[string]any{"x": secretShaped[0]}, "list": secretShaped})
		// a record too large to keep is replaced by a marker, not cut in the middle of a value
		l.Record("secret.big", Fields{"big": strings.Repeat("x", maxFields) + secretShaped[0]})
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range secretShaped {
			if strings.Contains(string(raw), s) || strings.Contains(string(raw), strings.TrimPrefix(s, "Authorization: Bearer ")) {
				t.Fatalf("unlocked=%v: %q is in the audit file in plain text:\n%s", unlocked, s, raw)
			}
		}
		if !strings.Contains(string(raw), "secret.test") {
			t.Fatalf("unlocked=%v: the event name is missing", unlocked)
		}
		if !unlocked && strings.Contains(string(raw), `"sealed"`) {
			t.Fatal("a locked log wrote detail fields")
		}
		if unlocked && !strings.Contains(string(raw), `"sealed"`) {
			t.Fatal("an unlocked log wrote no sealed detail")
		}
	}
}
