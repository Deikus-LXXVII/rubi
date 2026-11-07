package setup

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SelfTestFailedReason is the self-test step's reason when roles failed.
// One role names that role's cause. More than one role says how many failed.
// The sandbox and the real Home both call this, so the sentence cannot drift.
func SelfTestFailedReason(failed, total int, cause string) string {
	if total <= 1 {
		c := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(cause), "."))
		if c == "" {
			c = "it did not pass"
		} else if r, size := utf8.DecodeRuneInString(c); size > 0 && unicode.IsUpper(r) {
			c = string(unicode.ToLower(r)) + c[size:]
		}
		return "The self-test failed: " + c + "."
	}
	if failed < 1 {
		failed = 1
	}
	return fmt.Sprintf("%d of %d roles failed.", failed, total)
}
