// Package e2e implements the end-to-end encrypted channel between the panel and Rubi.
//
// The transport (a public tunnel) terminates TLS, so this layer is what keeps the tunnel provider from
// reading anything. Every request is sealed to the instance's pinned X25519 key with a fresh ephemeral
// key pair; the response is sealed with a key only the real instance can derive, which authenticates
// Rubi to the panel. Only WebCrypto primitives are used, so the panel needs no crypto library:
//
//	shared   = X25519(ephemeral_private, instance_public)
//	okm      = HKDF-SHA256(ikm=shared, salt=ephemeral_public||instance_public, info="rubi e2e v1", 64 bytes)
//	req_key  = okm[0:32]   resp_key = okm[32:64]
//	request  = AES-256-GCM(req_key,  nonce, body, ad="rubi-req|v1|"  + path)
//	response = AES-256-GCM(resp_key, nonce, body, ad="rubi-resp|v1|" + request_id)
//
// All binary fields are base64url without padding.
package e2e

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"

	"golang.org/x/crypto/hkdf"
)

const Version = 1

var b64 = base64.RawURLEncoding

// ErrOpen is deliberately uninformative: decryption failures reveal nothing.
var ErrOpen = errors.New("e2e: cannot open message")

type Request struct {
	V     int    `json:"v"`
	EPK   string `json:"epk"`
	Nonce string `json:"nonce"`
	CT    string `json:"ct"`
}

type Response struct {
	Nonce string `json:"nonce"`
	CT    string `json:"ct"`
}

func deriveKeys(shared, epk, spk []byte) (req, resp []byte, err error) {
	okm := make([]byte, 64)
	salt := append(append([]byte{}, epk...), spk...)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, salt, []byte("rubi e2e v1")), okm); err != nil {
		return nil, nil, err
	}
	return okm[:32], okm[32:], nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func seal(key []byte, ad string, plain []byte) (nonce, ct string, err error) {
	aead, err := gcm(key)
	if err != nil {
		return "", "", err
	}
	n := make([]byte, aead.NonceSize())
	if _, err := rand.Read(n); err != nil {
		return "", "", err
	}
	return b64.EncodeToString(n), b64.EncodeToString(aead.Seal(nil, n, plain, []byte(ad))), nil
}

func open(key []byte, ad, nonce, ct string) ([]byte, error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, ErrOpen
	}
	n, err1 := b64.DecodeString(nonce)
	c, err2 := b64.DecodeString(ct)
	if err1 != nil || err2 != nil || len(n) != aead.NonceSize() {
		return nil, ErrOpen
	}
	p, err := aead.Open(nil, n, c, []byte(ad))
	if err != nil {
		return nil, ErrOpen
	}
	return p, nil
}

func reqAD(path string) string { return "rubi-req|v1|" + path }
func respAD(rid string) string { return "rubi-resp|v1|" + rid }

// Server is Rubi's side of the channel.
type Server struct {
	Key *ecdh.PrivateKey
}

// Open decrypts a request addressed to path. It returns the plaintext and the key to seal the response.
func (s Server) Open(path string, r Request) (plain, respKey []byte, err error) {
	if r.V != Version {
		return nil, nil, ErrOpen
	}
	epkBytes, err := b64.DecodeString(r.EPK)
	if err != nil {
		return nil, nil, ErrOpen
	}
	epk, err := ecdh.X25519().NewPublicKey(epkBytes)
	if err != nil {
		return nil, nil, ErrOpen
	}
	shared, err := s.Key.ECDH(epk)
	if err != nil {
		return nil, nil, ErrOpen
	}
	reqKey, respKey, err := deriveKeys(shared, epkBytes, s.Key.PublicKey().Bytes())
	if err != nil {
		return nil, nil, ErrOpen
	}
	plain, err = open(reqKey, reqAD(path), r.Nonce, r.CT)
	if err != nil {
		return nil, nil, err
	}
	return plain, respKey, nil
}

// SealResponse encrypts a response bound to the request id.
func SealResponse(respKey []byte, rid string, plain []byte) (Response, error) {
	n, c, err := seal(respKey, respAD(rid), plain)
	return Response{Nonce: n, CT: c}, err
}

// Client is the panel's side of the channel. The web panel implements the same steps with WebCrypto;
// this Go version serves tests and the development client.
type Client struct {
	InstanceKey *ecdh.PublicKey
}

func (c Client) Seal(path string, plain []byte) (Request, []byte, error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Request{}, nil, err
	}
	shared, err := eph.ECDH(c.InstanceKey)
	if err != nil {
		return Request{}, nil, err
	}
	epk := eph.PublicKey().Bytes()
	reqKey, respKey, err := deriveKeys(shared, epk, c.InstanceKey.Bytes())
	if err != nil {
		return Request{}, nil, err
	}
	n, ct, err := seal(reqKey, reqAD(path), plain)
	if err != nil {
		return Request{}, nil, err
	}
	return Request{V: Version, EPK: b64.EncodeToString(epk), Nonce: n, CT: ct}, respKey, nil
}

func OpenResponse(respKey []byte, rid string, r Response) ([]byte, error) {
	return open(respKey, respAD(rid), r.Nonce, r.CT)
}

// ParseInstanceBundle extracts the X25519 public key from an identity public bundle
// (X25519 public || Ed25519 public, base64url).
func ParseInstanceBundle(bundle string) (*ecdh.PublicKey, error) {
	b, err := b64.DecodeString(bundle)
	if err != nil || len(b) != 64 {
		return nil, errors.New("e2e: bad instance key")
	}
	return ecdh.X25519().NewPublicKey(b[:32])
}
