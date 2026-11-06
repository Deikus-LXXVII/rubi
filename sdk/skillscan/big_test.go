package skillscan

import (
	"strings"
	"testing"
)

func TestBigInputIsFast(t *testing.T) {
	for _, in := range []string{strings.Repeat("a", 1<<20), strings.Repeat("ab12 ", 1<<18), strings.Repeat("x\n", 1<<19), strings.Repeat("password:", 1<<17)} {
		Scan("my-skill", []byte(okFM+in), nil)
	}
}
