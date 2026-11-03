package homeproto

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/identity"
)

func testBundle(t *testing.T) string {
	t.Helper()
	id, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	return id.PublicBundle()
}

func TestNewPINUsesTheUnambiguousAlphabet(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 300; i++ {
		p := NewPIN()
		if len(p) != PinLength {
			t.Fatalf("length %d", len(p))
		}
		for _, r := range p {
			if !strings.ContainsRune(PinAlphabet, r) {
				t.Fatalf("%q has %q", p, r)
			}
		}
		seen[p] = true
	}
	if len(seen) < 299 {
		t.Fatalf("only %d different PINs in 300", len(seen))
	}
	if strings.ContainsAny(PinAlphabet, "01OIL") {
		t.Fatal("ambiguous characters")
	}
}

func TestFormatAndNormalizePIN(t *testing.T) {
	if got := FormatPIN("ABCD2345"); got != "ABCD-2345" {
		t.Fatal(got)
	}
	if got := NormalizePIN(" abcd-2345 \n"); got != "ABCD2345" {
		t.Fatal(got)
	}
}

func TestLinkCodeHasNoSecretAndTakesThePIN(t *testing.T) {
	full := Code{Relay: "n", Bundle: testBundle(t), Secret: "the-long-secret", Name: "Mac mini"}
	link := full.WithoutSecret().String()
	if strings.Contains(link, "the-long-secret") {
		t.Fatal("secret in the link code")
	}
	if full.Secret == "" {
		t.Fatal("WithoutSecret changed the original")
	}
	if _, err := ParseCode(link); err == nil {
		t.Fatal("ParseCode accepted a code with no secret")
	}
	if _, err := ParseCodeWithPin(link, ""); err == nil || !strings.Contains(err.Error(), "PIN") {
		t.Fatalf("no PIN: %v", err)
	}
	c, err := ParseCodeWithPin(link, "abcd-2345")
	if err != nil || c.Secret != "ABCD2345" || c.Name != "Mac mini" {
		t.Fatalf("%+v %v", c, err)
	}
	// a code with its own secret ignores the PIN
	c, err = ParseCodeWithPin(full.String(), "zzzz-zzzz")
	if err != nil || c.Secret != "the-long-secret" {
		t.Fatalf("%+v %v", c, err)
	}
}
