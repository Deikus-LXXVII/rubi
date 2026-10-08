package e2e

import (
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"testing"
)

func pair(t *testing.T) (Server, Client) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Server{Key: k}, Client{InstanceKey: k.PublicKey()}
}

func TestRoundTrip(t *testing.T) {
	s, c := pair(t)
	req, clientResp, err := c.Seal("/v1/rpc", []byte(`{"op":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	plain, serverResp, err := s.Open("/v1/rpc", req)
	if err != nil || string(plain) != `{"op":"hello"}` {
		t.Fatalf("open: %q %v", plain, err)
	}
	resp, err := SealResponse(serverResp, "rid1", []byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenResponse(clientResp, "rid1", resp)
	if err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("response: %q %v", got, err)
	}
	if _, err := OpenResponse(clientResp, "other-rid", resp); !errors.Is(err, ErrOpen) {
		t.Fatal("response accepted for a different request id")
	}
}

func TestWrongInstanceOrPathOrTamper(t *testing.T) {
	s, c := pair(t)
	other, _ := pair(t)
	req, _, _ := c.Seal("/v1/rpc", []byte("secret"))

	if _, _, err := other.Open("/v1/rpc", req); !errors.Is(err, ErrOpen) {
		t.Fatal("another instance opened the request")
	}
	if _, _, err := s.Open("/v1/other", req); !errors.Is(err, ErrOpen) {
		t.Fatal("request accepted on a different path")
	}
	tampered := req
	tampered.CT = req.CT[:len(req.CT)-2] + "AA"
	if _, _, err := s.Open("/v1/rpc", tampered); !errors.Is(err, ErrOpen) {
		t.Fatal("tampered request accepted")
	}

	// A fake server (e.g. a hijacked tunnel) can't produce a response the client accepts.
	_, clientResp, _ := c.Seal("/v1/rpc", []byte("x"))
	fakeKey := make([]byte, 32)
	fake, _ := SealResponse(fakeKey, "rid", []byte(`{"ok":true}`))
	if _, err := OpenResponse(clientResp, "rid", fake); !errors.Is(err, ErrOpen) {
		t.Fatal("client accepted a response from an impostor")
	}
}

// A v2 request can't be read with the instance key alone: a leaked identity.json plus recorded traffic
// reveal nothing, and a request for an earlier process's session is refused.
func TestSessionForwardSecrecy(t *testing.T) {
	ik, _ := ecdh.X25519().GenerateKey(rand.Reader)
	srv := Server{Key: ik, Session: NewSession()}
	c := Client{InstanceKey: ik.PublicKey(), SessionKey: srv.Session.PublicKey()}
	req, respKey, err := c.Seal("/v1/rpc", []byte("the vault key"))
	if err != nil {
		t.Fatal(err)
	}
	if !req.Forward() {
		t.Fatal("not a v2 request")
	}
	plain, sk, err := srv.Open("/v1/rpc", req)
	if err != nil || string(plain) != "the vault key" {
		t.Fatalf("open: %v %q", err, plain)
	}
	resp, _ := SealResponse(sk, "rid", []byte("ok"))
	if got, err := OpenResponse(respKey, "rid", resp); err != nil || string(got) != "ok" {
		t.Fatalf("response: %v %q", err, got)
	}
	// The attacker has the instance's stored key, not the session key that lived in memory.
	thief := Server{Key: ik, Session: NewSession()}
	if _, _, err := thief.Open("/v1/rpc", req); err == nil {
		t.Fatal("opened without the session key")
	}
	thief.Session = nil
	if _, _, err := thief.Open("/v1/rpc", req); err == nil {
		t.Fatal("opened with the instance key alone")
	}
}
