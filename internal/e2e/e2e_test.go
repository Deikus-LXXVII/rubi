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
