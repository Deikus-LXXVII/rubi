package integrity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func TestCompare(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	sums := []byte("aaaa  rubi-v1.0.0-linux-amd64.tar.gz\nbbbb  bin/rubi-linux-amd64\n")
	sig := ed25519.Sign(priv, sums)
	base := Result{Version: "v1.0.0"}

	if r := compare(base, pubPEM, sums, sig, "bbbb", "bin/rubi-linux-amd64"); r.Status != "verified" {
		t.Fatalf("genuine: %+v", r)
	}
	if r := compare(base, pubPEM, sums, sig, "cccc", "bin/rubi-linux-amd64"); r.Status != "modified" {
		t.Fatalf("patched binary: %+v", r)
	}
	forged := []byte("cccc  bin/rubi-linux-amd64\n")
	if r := compare(base, pubPEM, forged, sig, "cccc", "bin/rubi-linux-amd64"); r.Status != "modified" {
		t.Fatalf("forged checksums: %+v", r)
	}
	if r := compare(base, pubPEM, sums, sig, "bbbb", "bin/rubi-linux-riscv64"); r.Status != "unknown" {
		t.Fatalf("missing platform: %+v", r)
	}
}
