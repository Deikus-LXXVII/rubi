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
	c, err := libsig.Verify(pub, l.ID, l.Resolved, l.SHA256, l.Sig)
	if err != nil {
		return LibraryFacts{}, err
	}
	if c.AdmittedBy == "" {
		return LibraryFacts{}, fmt.Errorf("the signature names no curator")
	}
	return LibraryFacts{Acked: c.Acked}, nil
}
