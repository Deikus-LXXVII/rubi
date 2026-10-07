// Package webauthn verifies passkey assertions used to approve actions.
//
// It implements only what Rubi needs from the WebAuthn spec (§7.2 "Verifying an Authentication
// Assertion"): the panel registers a passkey and hands Rubi its public key (SubjectPublicKeyInfo, as
// returned by AuthenticatorAttestationResponse.getPublicKey()); later, each approval is a signature over
// a challenge that Rubi derived from the exact action.
package webauthn

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// COSE algorithm identifiers the panel may register.
const (
	AlgES256 = -7
	AlgEdDSA = -8
	AlgRS256 = -257
)

const (
	flagUserPresent  = 0x01
	flagUserVerified = 0x04
)

type Assertion struct {
	CredentialID      string `json:"credential_id"`      // base64url
	ClientDataJSON    string `json:"client_data_json"`   // base64url
	AuthenticatorData string `json:"authenticator_data"` // base64url
	Signature         string `json:"signature"`          // base64url
}

type Expect struct {
	Challenge []byte
	Origin    string // e.g. https://rubi-panel.com
	RPID      string // e.g. rubi-panel.com
	PublicKey []byte // SPKI DER
	Alg       int
	SignCount uint32 // last seen counter; 0 means the authenticator doesn't count
}

var b64 = base64.RawURLEncoding

// Verify checks the assertion and returns the new signature counter.
func Verify(a Assertion, e Expect) (uint32, error) {
	clientData, err := decode(a.ClientDataJSON)
	if err != nil {
		return 0, err
	}
	authData, err := decode(a.AuthenticatorData)
	if err != nil {
		return 0, err
	}
	sig, err := decode(a.Signature)
	if err != nil {
		return 0, err
	}

	var cd struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		Origin    string `json:"origin"`
	}
	if err := json.Unmarshal(clientData, &cd); err != nil {
		return 0, errors.New("webauthn: bad client data")
	}
	if cd.Type != "webauthn.get" {
		return 0, errors.New("webauthn: not an assertion")
	}
	got, err := b64.DecodeString(cd.Challenge)
	if err != nil || subtle.ConstantTimeCompare(got, e.Challenge) != 1 {
		return 0, errors.New("webauthn: challenge mismatch (the approval was for something else)")
	}
	if cd.Origin != e.Origin {
		return 0, fmt.Errorf("webauthn: unexpected origin %q", cd.Origin)
	}

	if len(authData) < 37 {
		return 0, errors.New("webauthn: authenticator data too short")
	}
	rpHash := sha256.Sum256([]byte(e.RPID))
	if !bytes.Equal(authData[:32], rpHash[:]) {
		return 0, errors.New("webauthn: wrong relying party")
	}
	flags := authData[32]
	if flags&flagUserPresent == 0 || flags&flagUserVerified == 0 {
		return 0, errors.New("webauthn: user verification (Face ID, Touch ID or device PIN) is required")
	}
	count := binary.BigEndian.Uint32(authData[33:37])
	if (count != 0 || e.SignCount != 0) && count <= e.SignCount {
		return 0, errors.New("webauthn: signature counter went backwards (possible cloned authenticator)")
	}

	cdHash := sha256.Sum256(clientData)
	signed := append(append([]byte{}, authData...), cdHash[:]...)
	if err := verifySig(e.PublicKey, e.Alg, signed, sig); err != nil {
		return 0, err
	}
	return count, nil
}

func verifySig(spki []byte, alg int, signed, sig []byte) error {
	pub, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return errors.New("webauthn: bad stored public key")
	}
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if alg != AlgES256 {
			return errors.New("webauthn: algorithm mismatch")
		}
		h := sha256.Sum256(signed)
		if !ecdsa.VerifyASN1(k, h[:], sig) {
			return errors.New("webauthn: bad signature")
		}
	case ed25519.PublicKey:
		if alg != AlgEdDSA || !ed25519.Verify(k, signed, sig) {
			return errors.New("webauthn: bad signature")
		}
	case *rsa.PublicKey:
		if alg != AlgRS256 {
			return errors.New("webauthn: algorithm mismatch")
		}
		h := sha256.Sum256(signed)
		if rsa.VerifyPKCS1v15(k, crypto.SHA256, h[:], sig) != nil {
			return errors.New("webauthn: bad signature")
		}
	default:
		return errors.New("webauthn: unsupported key type")
	}
	return nil
}

func decode(s string) ([]byte, error) {
	b, err := b64.DecodeString(s)
	if err != nil {
		return nil, errors.New("webauthn: bad encoding")
	}
	return b, nil
}
