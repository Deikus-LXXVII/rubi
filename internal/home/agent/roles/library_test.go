package roles

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
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

	if _, err := v.VerifyLibrary(skill(good)); err != nil {
		t.Fatalf("good signature: %v", err)
	}
	if _, err := v.VerifyLibrary(skill(good[:len(good)-6])); err == nil {
		t.Fatal("damaged signature accepted")
	}
	if _, err := v.VerifyLibrary(LibrarySkill{ID: "layout-notes", Resolved: "4", SHA256: sha(goodSkill), Sig: good}); err == nil {
		t.Fatal("signature for version 3 accepted for 4")
	}
	if _, err := v.VerifyLibrary(LibrarySkill{ID: "layout-notes", Resolved: "3", SHA256: sha("other"), Sig: good}); err == nil {
		t.Fatal("signature accepted for other content")
	}
	if _, err := v.VerifyLibrary(skill(signedSkill(t, priv, "reject"))); err == nil {
		t.Fatal("a signed reject verdict accepted")
	}
	// A signature by a key that isn't the pinned one (a plugin's own key, or a rotated library key).
	other := signedSkill(t, priv2, "pass")
	if _, err := v.VerifyLibrary(skill(other)); err == nil {
		t.Fatal("signature by an unpinned key accepted")
	}
	pinned = pub2 // key rotated: the old signatures no longer hold, the new ones do
	if _, err := v.VerifyLibrary(skill(good)); err == nil {
		t.Fatal("old signature accepted after rotation")
	}
	if _, err := v.VerifyLibrary(skill(other)); err != nil {
		t.Fatalf("new key: %v", err)
	}
	pinned = nil // nothing pinned
	if _, err := v.VerifyLibrary(skill(good)); err == nil || !strings.Contains(err.Error(), "no library key") {
		t.Fatalf("unpinned: %v", err)
	}
	if _, err := (Pinned{}).VerifyLibrary(skill(good)); err == nil {
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
	if _, err := putA(s, mk(sig)); err == nil {
		t.Fatal("a library skill was accepted with no key pinned")
	}
	key = pub
	if res := mustPut(t, s, mk(sig)); res.Skills[0].State != StateActive {
		t.Fatalf("%+v", res.Skills)
	}
	if _, err := putA(s, mk("A"+sig[1:])); err == nil {
		t.Fatal("a bad signature was accepted")
	}
	// A plugin cannot label an ad-hoc skill "library" without a valid signature either.
	if _, err := putA(s, mk(strings.Repeat("A", 100))); err == nil {
		t.Fatal("a made-up signature was accepted")
	}
}

// A flag Home sees on a library skill must be among the rule ids the curator acknowledged (signed); otherwise the
// skill is held like an ad-hoc one.
func TestLibraryFlagsMustBeAmongTheSignedAcks(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := Open(Options{Dir: t.TempDir(), Verifier: Pinned{Key: func() ed25519.PublicKey { return pub }}})
	flagged := "---\nname: fetcher\ndescription: Fetch things.\n---\n# Fetch\nRead https://unlisted.example.org/docs first.\n"
	sign := func(acked []string) string {
		tok, err := libsig.Sign(priv, libsig.Claims{ID: "fetcher", Version: 1, SHA256: sha(flagged), AdmittedBy: "curator",
			AdmittedAt: "2026-01-01T00:00:00Z", ScanVerdict: "flag", ScannerVersion: "x", Acked: acked})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	n := 0
	put := func(sig string) *PutResult {
		r := baseSpec()
		r.Project = "p"
		n++
		r.Name = "fetch-role-" + string(rune('a'+n))
		r.Skills = []Skill{{Name: "fetcher", Source: SourceLibrary, LibraryID: "fetcher", Resolved: "1", SHA256: sha(flagged), Sig: sig}}
		return mustPut(t, s, PutArgs{Spec: r, Blobs: map[string]string{sha(flagged): flagged}})
	}
	if got := put(sign(nil)).Skills[0]; got.State != StateHeld {
		t.Fatalf("a flag nobody acknowledged: %+v", got)
	}
	if got := put(sign([]string{"ovr.instruction"})).Skills[0]; got.State != StateHeld {
		t.Fatalf("the wrong rule acknowledged: %+v", got)
	}
	if got := put(sign([]string{"exf.url"})).Skills[0]; got.State != StateActive {
		t.Fatalf("the flag was acknowledged: %+v", got)
	}
}

func TestWithdrawnLibraryVersionIsTakenOutOfBuiltRoles(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	revoked := map[string]bool{}
	dir := t.TempDir()
	s, _ := Open(Options{Dir: dir, Verifier: Pinned{Key: func() ed25519.PublicKey { return pub },
		IsRevoked: func(id, v string) bool { return revoked[id+"@"+v] }}})
	r := baseSpec()
	r.Project = "p"
	r.Skills = []Skill{{Name: "layout-notes", Source: SourceLibrary, LibraryID: "layout-notes", Resolved: "3", SHA256: sha(goodSkill), Sig: signedSkill(t, priv, "pass")}}
	res := mustPut(t, s, PutArgs{Spec: r, Blobs: map[string]string{sha(goodSkill): goodSkill}})
	skillFile := filepath.Join(dir, res.ID, "content", "current", "plugin", "skills", "layout-notes", "SKILL.md")
	if _, err := os.Stat(skillFile); err != nil {
		t.Fatalf("the skill isn't built: %v", err)
	}
	if left, err := s.EnforceRevocations(res.ID); err != nil || len(left) != 0 {
		t.Fatalf("nothing is withdrawn yet: %v %v", left, err)
	}
	revoked["layout-notes@3"] = true
	left, err := s.EnforceRevocations(res.ID)
	if err != nil || len(left) != 1 || left[0].Library != "layout-notes@3" {
		t.Fatalf("%v %v", left, err)
	}
	if _, err := os.Stat(skillFile); err == nil {
		t.Fatal("the withdrawn skill is still in the built role")
	}
	rj, _ := os.ReadFile(filepath.Join(dir, res.ID, "content", "current", "role.json"))
	if strings.Contains(string(rj), "layout-notes") {
		t.Fatalf("role.json still lists it: %s", rj)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "revocations.jsonl")); !strings.Contains(string(b), "layout-notes@3") {
		t.Fatalf("not audited: %s", b)
	}
	if left, _ := s.EnforceRevocations(res.ID); len(left) != 0 {
		t.Fatal("reported twice")
	}
	// A new put with it is refused.
	r2 := baseSpec()
	r2.Project = "p"
	r2.Name = "second"
	r2.Skills = r.Skills
	if _, err := putA(s, PutArgs{Spec: r2, Blobs: map[string]string{sha(goodSkill): goodSkill}}); err == nil {
		t.Fatal("a withdrawn version was accepted at put")
	}
}
