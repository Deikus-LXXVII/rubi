package roles

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// MaxBlobBytes caps the skill text a role keeps in its blobs (every version's skills stay as history).
const MaxBlobBytes = 4 << 20

// used is the index of what a role's versions refer to, kept in <role>/used.json so that a put and the GC don't
// read every version: the skill blobs with their sizes, and the approval ids already spent (approvals.go).
// Version is the highest version it covers; when it differs from the committed one (a crash between the version file
// and this file, or a damaged file) the index is rebuilt from the version files.
type used struct {
	Version   int                    `json:"version"`
	Blobs     map[string]int         `json:"blobs"`
	Approvals map[string]approvalUse `json:"approvals"`
}

func newUsed() *used {
	return &used{Blobs: map[string]int{}, Approvals: map[string]approvalUse{}}
}

func (u *used) blobBytes() int {
	n := 0
	for _, sz := range u.Blobs {
		n += sz
	}
	return n
}

// addVersion folds version r (whose predecessor is prev, nil for the first) into the index.
func (u *used) addVersion(prev, r *Role, size func(sha string) int) {
	for _, sk := range r.Skills {
		if _, ok := u.Blobs[sk.SHA256]; !ok {
			u.Blobs[sk.SHA256] = size(sk.SHA256)
		}
	}
	registerApprovals(u, r.Version, additions(prev, r))
	u.Version = r.Version
}

func (s *Store) usedPath(id string) string { return filepath.Join(s.roleDir(id), "used.json") }

// loadUsed returns the index for a role whose committed version is n, rebuilding it when it is missing or stale.
func (s *Store) loadUsed(id string, n int) *used {
	if b, err := os.ReadFile(s.usedPath(id)); err == nil {
		var u used
		if json.Unmarshal(b, &u) == nil && u.Version == n && u.Blobs != nil && u.Approvals != nil {
			return &u
		}
	}
	return s.rebuildUsed(id, n)
}

func (s *Store) rebuildUsed(id string, n int) *used {
	u := newUsed()
	size := func(sha string) int {
		if !shaRE.MatchString(sha) {
			return 0
		}
		if st, err := os.Stat(filepath.Join(s.roleDir(id), "blobs", sha)); err == nil {
			return int(st.Size())
		}
		return 0
	}
	var prev *Role
	for _, v := range s.versionNumbers(id) {
		if v > n {
			break
		}
		r, err := s.readVersion(id, v)
		if err != nil {
			continue
		}
		u.addVersion(prev, r, size)
		prev = r
	}
	u.Version = n
	return u
}

func (s *Store) saveUsed(id string, u *used) error {
	return writeAtomic(s.usedPath(id), marshal(u, false), 0o600)
}
