package panelapi

import (
	"strings"
	"testing"
)

func TestPaddingBuckets(t *testing.T) {
	for n, want := range map[int]int{10: 1024, 1024: 1024, 1025: 2048, 40000: 65536, 70000: 131072} {
		if got := PadTarget(n); got != want {
			t.Errorf("PadTarget(%d) = %d, want %d", n, got, want)
		}
	}
	for _, r := range []reply{{OK: true}, {OK: false, Error: "x"}, {OK: true, Result: strings.Repeat("a", 3000)}} {
		if b := padded(r); len(b) != PadTarget(len(b)) {
			t.Errorf("padded reply is %d bytes, not a bucket size", len(b))
		}
	}
}
