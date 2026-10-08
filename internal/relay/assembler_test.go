package relay

import (
	"strings"
	"testing"
)

func TestAssemblerFloodDoesNotBlockOthers(t *testing.T) {
	a := newAssembler()
	// One sender opens many unfinished messages: only its newest few are kept.
	for i := range 500 {
		a.add("flood", part{V: 1, R: strings.Repeat("x", 1) + string(rune('a'+i%26)) + strings.Repeat("y", i%7) + string(rune(i)), I: 0, N: 64, D: "zz"})
	}
	if n := a.count("flood"); n > maxPendingPerSender {
		t.Fatalf("flood keeps %d pending messages", n)
	}
	// Another sender's two-part message still goes through.
	if _, _, done := a.add("panel", part{V: 1, R: "m", I: 0, N: 2, D: "he"}); done {
		t.Fatal("done too early")
	}
	got, _, done := a.add("panel", part{V: 1, R: "m", I: 1, N: 2, D: "llo"})
	if !done || got != "hello" {
		t.Fatalf("got %q %v", got, done)
	}
}

func TestAssemblerRejectsOversizedChunks(t *testing.T) {
	a := newAssembler()
	if _, _, done := a.add("s", part{V: 1, R: "m", I: 0, N: 1, D: strings.Repeat("a", chunkSize+1)}); done {
		t.Fatal("oversized chunk accepted")
	}
	if got, _, done := a.add("s", part{V: 1, R: "m", I: 0, N: 1, D: "ok"}); !done || got != "ok" {
		t.Fatal("single chunk not passed through")
	}
}

func TestAssemblerByteCap(t *testing.T) {
	a := newAssembler()
	big := strings.Repeat("a", chunkSize)
	for i := range 300 {
		a.add(string(rune('A'+i)), part{V: 1, R: "m", I: 0, N: 2, D: big})
	}
	if a.bytes > maxPendingBytes {
		t.Fatalf("pending bytes %d over the cap", a.bytes)
	}
}
