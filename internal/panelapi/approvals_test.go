package panelapi_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/core"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/internal/webauthn"
)

var b64 = base64.RawURLEncoding

type authenticator struct {
	key   *ecdsa.PrivateKey
	count uint32
}

func (a *authenticator) approver() vault.Approver {
	spki, _ := x509.MarshalPKIXPublicKey(&a.key.PublicKey)
	return vault.Approver{CredentialID: "cred-1", PublicKey: spki, Alg: webauthn.AlgES256, Label: "iPhone"}
}

func (a *authenticator) sign(origin string) func([]byte) webauthn.Assertion {
	return func(challenge []byte) webauthn.Assertion {
		a.count++
		rp := sha256.Sum256([]byte("rubi-panel.com"))
		auth := binary.BigEndian.AppendUint32(append(rp[:], 0x05), a.count)
		cd, _ := json.Marshal(map[string]string{"type": "webauthn.get", "challenge": b64.EncodeToString(challenge), "origin": origin})
		h := sha256.Sum256(cd)
		d := sha256.Sum256(append(append([]byte{}, auth...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, a.key, d[:])
		return webauthn.Assertion{CredentialID: "cred-1", ClientDataJSON: b64.EncodeToString(cd),
			AuthenticatorData: b64.EncodeToString(auth), Signature: b64.EncodeToString(sig)}
	}
}

func submit(t *testing.T, c *core.Core, runs *atomic.Int32, chosen *string) string {
	t.Helper()
	out, err := c.Approvals.Submit(context.Background(), approvals.Request{
		Integration: "dev", Kind: "dev.send", Summary: "Send email to anna@example.com",
		Preview: map[string]string{"to": "anna@example.com", "subject": "Meeting"},
		Options: []approvals.Option{{Key: "send", Label: "Send"}, {Key: "send_track", Label: "Send and notify on reply"}},
		Execute: func(_ context.Context, opt string) (any, error) { runs.Add(1); *chosen = opt; return "sent", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["level"] != approvals.Strong || !strings.Contains(out["approval_url"].(string), "a=approve%3Aapr_") {
		t.Fatalf("submit: %v", out)
	}
	return out["approval_id"].(string)
}

func TestStrongApprovals(t *testing.T) {
	c, _ := setup(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	dev := &authenticator{key: key}
	if _, err := client(t, c, "pair").PairWithPassword("correct horse", dev.approver()); err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	var chosen string

	// Passkey approval of the "send and notify" option.
	id := submit(t, c, &runs, &chosen)
	pc := client(t, c, "approve:"+id)
	info, err := pc.ApprovalInfo(id)
	if err != nil || len(info.Challenges) != 2 || !info.Password {
		t.Fatalf("info: %+v %v", info, err)
	}
	// A signature made for one option can't approve another: sign "send", submit as "send_track".
	sig := dev.sign("https://rubi-panel.com")
	if _, err := pc.ApproveWithPasskey(id, "send_track", func(_ []byte) webauthn.Assertion {
		ch, _ := b64.DecodeString(info.Challenges["send"])
		return sig(ch)
	}); err == nil {
		t.Fatal("assertion for another option accepted")
	}
	if _, err := pc.ApproveWithPasskey(id, "send_track", dev.sign("https://evil.example")); err == nil {
		t.Fatal("assertion from a foreign origin accepted")
	}
	if runs.Load() != 0 {
		t.Fatal("executed without a valid proof")
	}
	if _, err := pc.ApproveWithPasskey(id, "send_track", dev.sign("https://rubi-panel.com")); err != nil {
		t.Fatal(err)
	}
	if runs.Load() != 1 || chosen != "send_track" {
		t.Fatalf("runs=%d chosen=%s", runs.Load(), chosen)
	}

	// Tickets are bound to one approval.
	id2 := submit(t, c, &runs, &chosen)
	if _, err := pc.ApprovalInfo(id2); err == nil {
		t.Fatal("ticket for one approval opened another")
	}
	pc2 := client(t, c, "approve:"+id2)
	if _, err := pc2.ApproveWithPassword(id2, "send", "wrong password"); err == nil {
		t.Fatal("wrong password approved")
	}
	if _, err := pc2.ApproveWithPassword(id2, "send", "correct horse"); err != nil || runs.Load() != 2 || chosen != "send" {
		t.Fatalf("password approval: %v runs=%d", err, runs.Load())
	}

	// Deny needs no proof and executes nothing.
	id3 := submit(t, c, &runs, &chosen)
	if out, err := client(t, c, "approve:"+id3).Deny(id3); err != nil || out["state"] != "denied" || runs.Load() != 2 {
		t.Fatalf("deny: %v %v", out, err)
	}
}
