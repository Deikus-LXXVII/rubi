package roles

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/Deikus-LXXVII/rubi/internal/library/libsig"
)

// Pinned verifies library skills against the library public key this computer pinned (Rubi's core presents it
// with the core-only op library.key). With no key pinned every library skill is refused.
type Pinned struct {
	Key func() ed25519.PublicKey
	// IsRevoked says whether Rubi withdrew this version from the library (the signed list Rubi's core pushes with
	// library.revoked). Nil: nothing is.
	IsRevoked func(id, version string) bool
}

// Revoked says whether a library version was withdrawn.
func (p Pinned) Revoked(id, version string) bool {
	return p.IsRevoked != nil && p.IsRevoked(id, version)
}

// Revoker is a Verifier that also knows withdrawn versions: the store leaves them out of every role it builds.
type Revoker interface {
	Revoked(id, version string) bool
}

// VerifyLibrary checks the signature on skill l.
func (p Pinned) VerifyLibrary(l LibrarySkill) (LibraryFacts, error) {
	var pub ed25519.PublicKey
	if p.Key != nil {
		pub = p.Key()
	}
	if len(pub) != ed25519.PublicKeySize {
		return LibraryFacts{}, errors.New("no library key is pinned on this computer yet; Rubi presents it when the skill is fetched")
	}
	if p.Revoked(l.ID, l.Resolved) {
		return LibraryFacts{}, errors.New("this skill version was withdrawn from the library")
	}
	c, err := libsig.Verify(pub, l.ID, l.Resolved, l.SHA256, l.Sig)
	if err != nil {
		return LibraryFacts{}, err
	}
	if c.AdmittedBy == "" {
		return LibraryFacts{}, fmt.Errorf("the signature names no curator")
	}
	return LibraryFacts{Acked: c.Acked}, nil
}
