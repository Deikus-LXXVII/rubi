package roles

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/library/libsig"
)

func signedSkill(t *testing.T, priv ed25519.PrivateKey, verdict string) string {
	t.Helper()
	tok, err := libsig.Sign(priv, libsig.Claims{ID: "layout-notes", Version: 3, SHA256: sha(goodSkill), AdmittedBy: "curator",
		AdmittedAt: "2026-01-01T00:00:00Z", ScanVerdict: verdict, ScannerVersion: "skillscan/2"})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestPinnedVerifier(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
	skill := func(sig string) LibrarySkill {
		return LibrarySkill{ID: "layout-notes", Resolved: "3", SHA256: sha(goodSkill), Sig: sig}
	}
	good := signedSkill(t, priv, "pass")
	pinned := pub
	v := Pinned{Key: func() ed25519.PublicKey { return pinned }}

	if err := v.VerifyLibrary(skill(good)); err != nil {
		t.Fatalf("good signature: %v", err)
	}
	if err := v.VerifyLibrary(skill(good[:len(good)-6])); err == nil {
		t.Fatal("damaged signature accepted")
	}
	if err := v.VerifyLibrary(LibrarySkill{ID: "layout-notes", Resolved: "4", SHA256: sha(goodSkill), Sig: good}); err == nil {
		t.Fatal("signature for version 3 accepted for 4")
	}
	if err := v.VerifyLibrary(LibrarySkill{ID: "layout-notes", Resolved: "3", SHA256: sha("other"), Sig: good}); err == nil {
		t.Fatal("signature accepted for other content")
	}
	if err := v.VerifyLibrary(skill(signedSkill(t, priv, "reject"))); err == nil {
		t.Fatal("a signed reject verdict accepted")
	}
	// A signature by a key that isn't the pinned one (a plugin's own key, or a rotated library key).
	other := signedSkill(t, priv2, "pass")
	if err := v.VerifyLibrary(skill(other)); err == nil {
		t.Fatal("signature by an unpinned key accepted")
	}
	pinned = pub2 // key rotated: the old signatures no longer hold, the new ones do
	if err := v.VerifyLibrary(skill(good)); err == nil {
		t.Fatal("old signature accepted after rotation")
	}
	if err := v.VerifyLibrary(skill(other)); err != nil {
		t.Fatalf("new key: %v", err)
	}
	pinned = nil // nothing pinned
	if err := v.VerifyLibrary(skill(good)); err == nil || !strings.Contains(err.Error(), "no library key") {
		t.Fatalf("unpinned: %v", err)
	}
	if err := (Pinned{}).VerifyLibrary(skill(good)); err == nil {
		t.Fatal("a Pinned without a key function accepted")
	}
}

func TestPutWithPinnedVerifier(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var key ed25519.PublicKey
	s, _ := Open(Options{Dir: t.TempDir(), Verifier: Pinned{Key: func() ed25519.PublicKey { return key }}})
	mk := func(sig string) PutArgs {
		r := baseSpec()
		r.Project = "p"
		r.Skills = []Skill{{Name: "layout-notes", Source: SourceLibrary, LibraryID: "layout-notes", Resolved: "3", SHA256: sha(goodSkill), Sig: sig}}
		return PutArgs{Spec: r, Blobs: map[string]string{sha(goodSkill): goodSkill}}
	}
	sig := signedSkill(t, priv, "pass")
	if _, err := s.Put(mk(sig)); err == nil {
		t.Fatal("a library skill was accepted with no key pinned")
	}
	key = pub
	if res := mustPut(t, s, mk(sig)); res.Skills[0].State != StateActive {
		t.Fatalf("%+v", res.Skills)
	}
	if _, err := s.Put(mk("A" + sig[1:])); err == nil {
		t.Fatal("a bad signature was accepted")
	}
	// A plugin cannot label an ad-hoc skill "library" without a valid signature either.
	if _, err := s.Put(mk(strings.Repeat("A", 100))); err == nil {
		t.Fatal("a made-up signature was accepted")
	}
}
