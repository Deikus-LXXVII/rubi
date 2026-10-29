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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/core"
	"github.com/Deikus-LXXVII/rubi/internal/panelclient"
	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/internal/webauthn"
)

var b64 = base64.RawURLEncoding

type authenticator struct {
	key   *ecdsa.PrivateKey
	count uint32
	flags byte // authenticator data flags; 0 means 0x05 (user present and verified)
}

func (a *authenticator) approver() vault.Approver {
	spki, _ := x509.MarshalPKIXPublicKey(&a.key.PublicKey)
	return vault.Approver{CredentialID: "cred-1", PublicKey: spki, Alg: webauthn.AlgES256, Label: "iPhone"}
}

func (a *authenticator) sign(origin string) func([]byte) webauthn.Assertion {
	return func(challenge []byte) webauthn.Assertion {
		a.count++
		rp := sha256.Sum256([]byte("rubi-panel.com"))
		flags := a.flags
		if flags == 0 {
			flags = 0x05
		}
		auth := binary.BigEndian.AppendUint32(append(rp[:], flags), a.count)
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
	return submitKind(t, c, "dev.send", approvals.Strong, runs, chosen)
}

func submitKind(t *testing.T, c *core.Core, kind string, level approvals.Level, runs *atomic.Int32, chosen *string) string {
	t.Helper()
	out, err := c.Approvals.Submit(context.Background(), approvals.Request{
		Integration: strings.Split(kind, ".")[0], Kind: kind, Summary: "Send email to anna@example.com",
		Preview: map[string]string{"to": "anna@example.com", "subject": "Meeting"},
		Options: []approvals.Option{{Key: "send", Label: "Send"}, {Key: "send_track", Label: "Send and notify on reply"}},
		Execute: func(_ context.Context, opt string) (any, error) { runs.Add(1); *chosen = opt; return "sent", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["level"] != level || !strings.Contains(out["approval_url"].(string), "a=approve%3Aapr_") {
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

// installManifest puts a plugin manifest in place without its package (enough for policy lookups).
func installManifest(t *testing.T, c *core.Core, raw string) {
	t.Helper()
	var m plugins.Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if err := plugins.Check(&m); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "stage")
	_ = os.MkdirAll(dir, 0o700)
	tree, _ := plugins.TreeHash(dir)
	if err := c.Store.Commit(&plugins.Candidate{Manifest: m, Dir: dir, Tree: tree}, ""); err != nil {
		t.Fatal(err)
	}
}

// TestTapApprovals: a passkey tap without user verification approves a tap-level action, and never a
// strong one; user presence is always required.
func TestTapApprovals(t *testing.T) {
	c, _ := setup(t)
	installManifest(t, c, `{"schema":1,"id":"dev","name":"Dev","version":"v1.0.0","api":1,"entry":"dev",
		"publisher":{"name":"x","key":"MCowBQYDK2VwAyEAxeDfKAkO77JdARN7Y2jJT3tXw9mN+GqqH8R5mhcxt8c="},
		"actions":[{"kind":"dev.safe","title":"Safe","default_level":"tap"},
		           {"kind":"dev.send","title":"Send","default_level":"strong"}]}`)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	dev := &authenticator{key: key}
	if _, err := client(t, c, "pair").PairWithPassword("correct horse", dev.approver()); err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	var chosen string
	sig := dev.sign("https://rubi-panel.com")

	// Strong: a tap is refused and the request stays pending; Face ID then approves it.
	id := submitKind(t, c, "dev.send", approvals.Strong, &runs, &chosen)
	pc := client(t, c, "approve:"+id)
	dev.flags = 0x01
	if _, err := pc.ApproveWithPasskey(id, "send", sig); err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("tap approved a strong request: %v", err)
	}
	if snap, _ := c.Approvals.Get(id); snap.State != approvals.Pending || runs.Load() != 0 {
		t.Fatalf("after a refused tap: %+v runs=%d", snap, runs.Load())
	}
	dev.flags = 0x05
	if out, err := pc.ApproveWithPasskey(id, "send", sig); err != nil || out["state"] != "executed" {
		t.Fatalf("verified passkey on strong: %v %v", out, err)
	}

	// Tap: no presence is refused; a tap approves; Face ID and the password do too.
	id = submitKind(t, c, "dev.safe", approvals.Tap, &runs, &chosen)
	pc = client(t, c, "approve:"+id)
	if info, err := pc.ApprovalInfo(id); err != nil || info.Approval.Level != string(approvals.Tap) {
		t.Fatalf("tap info: %+v %v", info, err)
	}
	dev.flags = 0x04 // verified but not present: not a real authenticator response
	if _, err := pc.ApproveWithPasskey(id, "send", sig); err == nil || !strings.Contains(err.Error(), "presence") {
		t.Fatalf("assertion without user presence approved: %v", err)
	}
	dev.flags = 0x01
	if out, err := pc.ApproveWithPasskey(id, "send", sig); err != nil || out["state"] != "executed" || runs.Load() != 2 {
		t.Fatalf("tap on tap: %v %v runs=%d", out, err, runs.Load())
	}
	dev.flags = 0x05
	id = submitKind(t, c, "dev.safe", approvals.Tap, &runs, &chosen)
	if out, err := client(t, c, "approve:"+id).ApproveWithPasskey(id, "send", sig); err != nil || out["state"] != "executed" {
		t.Fatalf("verified passkey on tap: %v %v", out, err)
	}
	id = submitKind(t, c, "dev.safe", approvals.Tap, &runs, &chosen)
	if out, err := client(t, c, "approve:"+id).ApproveWithPassword(id, "send", "correct horse"); err != nil || out["state"] != "executed" {
		t.Fatalf("password on tap: %v %v", out, err)
	}
}

// TestPendingApprovalList: a settings link lists what waits for the user in the panel and opens each one;
// links made for anything narrower can't, and decided or chat-level requests aren't listed.
func TestPendingApprovalList(t *testing.T) {
	c, _ := setup(t)
	installManifest(t, c, `{"schema":1,"id":"dev","name":"Dev","version":"v1.0.0","api":1,"entry":"dev",
		"publisher":{"name":"x","key":"MCowBQYDK2VwAyEAxeDfKAkO77JdARN7Y2jJT3tXw9mN+GqqH8R5mhcxt8c="},
		"actions":[{"kind":"dev.safe","title":"Safe","default_level":"tap"},
		           {"kind":"dev.send","title":"Send","default_level":"strong"},
		           {"kind":"dev.chat","title":"Chat","default_level":"chat"}]}`)
	if _, err := client(t, c, "pair").PairWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	var chosen string
	strong := submitKind(t, c, "dev.send", approvals.Strong, &runs, &chosen)
	tap := submitKind(t, c, "dev.safe", approvals.Tap, &runs, &chosen)
	decided := submitKind(t, c, "dev.send", approvals.Strong, &runs, &chosen)
	if _, err := client(t, c, "approve:"+decided).Deny(decided); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Approvals.Submit(context.Background(), approvals.Request{Integration: "dev", Kind: "dev.chat",
		Summary: "chat", Options: []approvals.Option{{Key: "ok", Label: "OK"}},
		Execute: func(context.Context, string) (any, error) { return nil, nil }}); err != nil {
		t.Fatal(err)
	}

	type item struct {
		ID      string `json:"approval_id"`
		Kind    string `json:"kind"`
		Level   string `json:"level"`
		Summary string `json:"summary"`
		Expires string `json:"expires_at"`
	}
	var list struct {
		Approvals []item `json:"approvals"`
	}
	sc := client(t, c, "settings")
	if err := sc.Call("approval.list", nil, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Approvals) != 2 || list.Approvals[0].ID != strong || list.Approvals[1].ID != tap ||
		list.Approvals[1].Level != "tap" || list.Approvals[0].Summary == "" || list.Approvals[0].Expires == "" {
		t.Fatalf("list: %+v", list)
	}
	// No preview, options or codes in the list: those come with approval.get, for one request.
	var raw map[string][]map[string]any
	_ = sc.Call("approval.list", nil, &raw)
	for _, k := range []string{"preview", "options", "code", "items"} {
		if _, ok := raw["approvals"][0][k]; ok {
			t.Fatalf("list carries %q: %v", k, raw)
		}
	}

	// Opening one gives a ticket for that request only, used like its own link.
	var open struct{ ApprovalID, Ticket string }
	if err := sc.Call("approval.open", map[string]string{"approval_id": strong}, &open); err != nil || open.Ticket == "" {
		t.Fatalf("open: %+v %v", open, err)
	}
	ac := *sc
	ac.Link.Ticket = open.Ticket
	if info, err := ac.ApprovalInfo(strong); err != nil || info.Approval.ID != strong {
		t.Fatalf("approval.get with the opened ticket: %+v %v", info, err)
	}
	if _, err := ac.ApprovalInfo(tap); err == nil {
		t.Fatal("a ticket opened for one request read another")
	}
	if out, err := ac.ApproveWithPassword(strong, "send", "correct horse"); err != nil || out["state"] != "executed" {
		t.Fatalf("approve: %v %v", out, err)
	}
	for _, id := range []string{strong, decided, "apr_nope"} {
		if err := sc.Call("approval.open", map[string]string{"approval_id": id}, &open); err == nil {
			t.Fatalf("opened %s, which isn't waiting", id)
		}
	}
	if err := sc.Call("approval.list", nil, &list); err != nil || len(list.Approvals) != 1 || list.Approvals[0].ID != tap {
		t.Fatalf("after approving: %+v %v", list, err)
	}

	// Only a settings link: not an approval link, nor the view-only settings it opens, nor setup or
	// agent links. Without a valid ticket nothing at all.
	view := client(t, c, "approve:"+tap)
	var vs struct {
		Ticket   string `json:"ticket"`
		ViewOnly bool   `json:"view_only"`
	}
	if err := view.Call("session.settings", nil, &vs); err != nil || !vs.ViewOnly {
		t.Fatalf("session.settings: %+v %v", vs, err)
	}
	viewOnly := *view
	viewOnly.Link.Ticket = vs.Ticket
	for name, pc := range map[string]*panelclient.Client{"approval link": view, "view-only settings": &viewOnly,
		"setup link": client(t, c, "setup:dev"), "agent link": client(t, c, "agent:Ann"), "unlock link": client(t, c, "unlock")} {
		if err := pc.Call("approval.list", nil, &list); err == nil {
			t.Fatalf("%s listed approvals", name)
		}
		if err := pc.Call("approval.open", map[string]string{"approval_id": tap}, &open); err == nil {
			t.Fatalf("%s opened an approval", name)
		}
	}
	expired := *sc
	expired.Link.Ticket = "not-a-ticket"
	if err := expired.Call("approval.list", nil, &list); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("no ticket: %v", err)
	}

	// Locked: nothing is listed (locking cancels what waited anyway).
	c.Lock()
	if err := sc.Call("approval.list", nil, &list); err == nil {
		t.Fatal("listed while locked")
	}
}
