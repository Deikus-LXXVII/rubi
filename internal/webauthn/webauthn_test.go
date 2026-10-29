package webauthn

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

// fakeAuthenticator signs assertions the way a platform authenticator does.
type fakeAuthenticator struct {
	key   *ecdsa.PrivateKey
	count uint32
}

func (f *fakeAuthenticator) spki() []byte {
	b, _ := x509.MarshalPKIXPublicKey(&f.key.PublicKey)
	return b
}

func (f *fakeAuthenticator) assert(t *testing.T, rpID, origin string, challenge []byte, flags byte) Assertion {
	t.Helper()
	f.count++
	rp := sha256.Sum256([]byte(rpID))
	auth := append(rp[:], flags)
	auth = binary.BigEndian.AppendUint32(auth, f.count)
	cd, _ := json.Marshal(map[string]string{"type": "webauthn.get", "challenge": b64.EncodeToString(challenge), "origin": origin})
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, auth...), h[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, f.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return Assertion{CredentialID: "cred", ClientDataJSON: b64.EncodeToString(cd),
		AuthenticatorData: b64.EncodeToString(auth), Signature: b64.EncodeToString(sig)}
}

func TestVerify(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f := &fakeAuthenticator{key: key}
	challenge := []byte("approve icloud-mail.send apr_1 send")
	exp := Expect{Challenge: challenge, Origin: "https://rubi-panel.com", RPID: "rubi-panel.com", PublicKey: f.spki(), Alg: AlgES256}

	n, err := Verify(f.assert(t, "rubi-panel.com", "https://rubi-panel.com", challenge, 0x05), exp)
	if err != nil || n != 1 {
		t.Fatalf("valid assertion rejected: %v", err)
	}
	exp.SignCount = n

	cases := map[string]Assertion{
		"challenge":     f.assert(t, "rubi-panel.com", "https://rubi-panel.com", []byte("something else"), 0x05),
		"origin":        f.assert(t, "rubi-panel.com", "https://evil.example", challenge, 0x05),
		"relying party": f.assert(t, "evil.example", "https://rubi-panel.com", challenge, 0x05),
		"verification":  f.assert(t, "rubi-panel.com", "https://rubi-panel.com", challenge, 0x01),
	}
	for name, a := range cases {
		if _, err := Verify(a, exp); err == nil {
			t.Errorf("%s: forged assertion accepted", name)
		}
	}

	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, err := Verify((&fakeAuthenticator{key: other, count: 10}).assert(t, "rubi-panel.com", "https://rubi-panel.com", challenge, 0x05), exp); err == nil {
		t.Error("assertion from another key accepted")
	}

	replay := f.assert(t, "rubi-panel.com", "https://rubi-panel.com", challenge, 0x05)
	exp.SignCount = 100
	if _, err := Verify(replay, exp); err == nil || !strings.Contains(err.Error(), "counter") {
		t.Errorf("stale counter accepted: %v", err)
	}
}

// A tap (user presence without verification) passes only VerifyTap, which says so; without presence
// nothing passes.
func TestVerifyTap(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f := &fakeAuthenticator{key: key}
	challenge := []byte("approve claude-code.run_safe apr_1 allow")
	exp := Expect{Challenge: challenge, Origin: "https://rubi-panel.com", RPID: "rubi-panel.com", PublicKey: f.spki(), Alg: AlgES256}
	tap := func(flags byte) Assertion {
		return f.assert(t, "rubi-panel.com", "https://rubi-panel.com", challenge, flags)
	}

	if _, err := Verify(tap(0x01), exp); err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("Verify accepted an assertion without user verification: %v", err)
	}
	n, uv, err := VerifyTap(tap(0x01), exp)
	if err != nil || uv || n == 0 {
		t.Fatalf("VerifyTap on a tap: n=%d uv=%v err=%v", n, uv, err)
	}
	exp.SignCount = n
	n, uv, err = VerifyTap(tap(0x05), exp)
	if err != nil || !uv {
		t.Fatalf("VerifyTap with verification: uv=%v err=%v", uv, err)
	}
	exp.SignCount = n
	for _, flags := range []byte{0x00, 0x04} {
		if _, _, err := VerifyTap(tap(flags), exp); err == nil || !strings.Contains(err.Error(), "presence") {
			t.Errorf("flags %#x: an assertion without user presence passed: %v", flags, err)
		}
	}
	// Everything else is checked as strictly as for Verify.
	if _, _, err := VerifyTap(f.assert(t, "rubi-panel.com", "https://rubi-panel.com", []byte("other"), 0x01), exp); err == nil {
		t.Error("VerifyTap accepted another challenge")
	}
	if _, _, err := VerifyTap(f.assert(t, "evil.example", "https://rubi-panel.com", challenge, 0x01), exp); err == nil {
		t.Error("VerifyTap accepted another relying party")
	}
	if _, _, err := VerifyTap(f.assert(t, "rubi-panel.com", "https://evil.example", challenge, 0x01), exp); err == nil {
		t.Error("VerifyTap accepted another origin")
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, _, err := VerifyTap((&fakeAuthenticator{key: other, count: 50}).assert(t, "rubi-panel.com", "https://rubi-panel.com", challenge, 0x01), exp); err == nil {
		t.Error("VerifyTap accepted another key")
	}
	exp.SignCount = 1000
	if _, _, err := VerifyTap(tap(0x01), exp); err == nil || !strings.Contains(err.Error(), "counter") {
		t.Errorf("VerifyTap accepted a stale counter: %v", err)
	}
}
