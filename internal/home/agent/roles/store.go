package roles

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options configure a Store. Dir is <helper dir>/agent-roles.
type Options struct {
	Dir string
	// Projects lists the agent project ids on this device (nil: any well-formed id is accepted).
	Projects func() []string
	// BaseHosts are the hosts every role may reach (config agent_allowed_hosts); the scan flags URLs outside them
	// and the role's own hosts.
	BaseHosts func() []string
	// Verifier checks library signatures (nil: Refuse).
	Verifier Verifier
	// MaxTotal is role_max_total (0: 50).
	MaxTotal int
	Now      func() time.Time
}

// Store is the role store.
type Store struct {
	opt Options

	mu       sync.Mutex // guards locks, pins, reserved
	locks    map[string]*sync.Mutex
	pins     map[string]map[int]int
	reserved map[string]string // project/name -> role id, while a put is committing
	nameMu   sync.Mutex        // serializes the name and total checks
}

// Open opens (and repairs) the store: every role is reconciled from its highest version file, and role
// directories that never got a version are removed.
func Open(opt Options) (*Store, error) {
	if opt.Dir == "" {
		return nil, errors.New("no directory for roles")
	}
	if opt.Verifier == nil {
		opt.Verifier = Refuse{}
	}
	if opt.MaxTotal <= 0 {
		opt.MaxTotal = MaxTotalDefault
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if err := os.MkdirAll(opt.Dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{opt: opt, locks: map[string]*sync.Mutex{}, pins: map[string]map[int]int{}, reserved: map[string]string{}}
	ents, err := os.ReadDir(opt.Dir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if !e.IsDir() || !IsID(e.Name()) {
			continue
		}
		unlock := s.lockRole(e.Name())
		err := s.reconcile(e.Name())
		unlock()
		if errors.Is(err, errNoRole) {
			_ = os.RemoveAll(s.roleDir(e.Name()))
		}
	}
	return s, nil
}

var errNoRole = errors.New("no such role")

func (s *Store) roleDir(id string) string { return filepath.Join(s.opt.Dir, id) } // id must be IsID

func (s *Store) baseHosts() []string {
	if s.opt.BaseHosts == nil {
		return nil
	}
	return s.opt.BaseHosts()
}

func (s *Store) lockRole(id string) func() {
	s.mu.Lock()
	l := s.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		s.locks[id] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Pin marks a version as in use (a live query reads it) so GC keeps its directory. Call the returned function to
// release it. Pins live in memory only.
func (s *Store) Pin(id string, version int) (release func()) {
	s.mu.Lock()
	if s.pins[id] == nil {
		s.pins[id] = map[int]int{}
	}
	s.pins[id][version]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.pins[id][version]--; s.pins[id][version] <= 0 {
				delete(s.pins[id], version)
			}
		})
	}
}

func (s *Store) pinned(id string) map[int]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int]bool{}
	for v := range s.pins[id] {
		out[v] = true
	}
	return out
}

// index is the cache in index.json.
type index struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Version     int    `json:"version"`
	ChangedBy   string `json:"changed_by"`
	ChangedAt   string `json:"changed_at"`
}

func (s *Store) readIndex(id string) (*index, error) {
	b, err := os.ReadFile(filepath.Join(s.roleDir(id), "index.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errNoRole
		}
		return nil, err
	}
	var ix index
	if json.Unmarshal(b, &ix) != nil || ix.ID != id || ix.Version < 1 {
		return nil, errors.New("index.json is damaged")
	}
	return &ix, nil
}

func (s *Store) readVersion(id string, n int) (*Role, error) {
	if n < 1 {
		return nil, fmt.Errorf("no version %d", n)
	}
	b, err := os.ReadFile(filepath.Join(s.roleDir(id), "versions", strconv.Itoa(n)+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("role %s has no version %d", id, n)
		}
		return nil, err
	}
	var r Role
	if json.Unmarshal(b, &r) != nil || r.ID != id || r.Version != n {
		return nil, fmt.Errorf("version %d of %s is damaged", n, id)
	}
	return &r, nil
}

var versionFileRE = regexp.MustCompile(`^([1-9][0-9]{0,5})\.json$`)
var contentDirRE = regexp.MustCompile(`^v([1-9][0-9]{0,5})$`)

// versionNumbers lists the numbers of the version files, ascending.
func (s *Store) versionNumbers(id string) []int {
	ents, _ := os.ReadDir(filepath.Join(s.roleDir(id), "versions"))
	var ns []int
	for _, e := range ents {
		if m := versionFileRE.FindStringSubmatch(e.Name()); m != nil {
			n, _ := strconv.Atoi(m[1])
			ns = append(ns, n)
		}
	}
	sort.Ints(ns)
	return ns
}

// blob reads a stored skill text and checks it against its name: the file is trusted for nothing.
func (s *Store) blob(id, sha string) ([]byte, error) {
	if !shaRE.MatchString(sha) {
		return nil, errors.New("bad blob name")
	}
	b, err := os.ReadFile(filepath.Join(s.roleDir(id), "blobs", sha))
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != sha {
		return nil, fmt.Errorf("blob %.12s doesn't match its hash", sha)
	}
	return b, nil
}

// reconcile makes the disk agree with the highest version file, under the role's lock.
func (s *Store) reconcile(id string) error {
	ns := s.versionNumbers(id)
	if len(ns) == 0 {
		return errNoRole
	}
	n := ns[len(ns)-1]
	r, err := s.readVersion(id, n)
	if err != nil {
		return err
	}
	dir := s.roleDir(id)
	for _, d := range []string{"versions", "blobs", "content", "claude"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return err
		}
	}
	// A crash can leave temp files and a content/v<m> with m above the committed version.
	ents, _ := os.ReadDir(filepath.Join(dir, "content"))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			_ = os.RemoveAll(filepath.Join(dir, "content", e.Name()))
		} else if m := contentDirRE.FindStringSubmatch(e.Name()); m != nil {
			if v, _ := strconv.Atoi(m[1]); v > n {
				_ = os.RemoveAll(filepath.Join(dir, "content", e.Name()))
			}
		}
	}
	vdir := filepath.Join(dir, "content", fmt.Sprintf("v%d", n))
	if st, err := os.Stat(filepath.Join(vdir, "role.json")); err != nil || !st.Mode().IsRegular() {
		if err := s.materialize(r, func(sha string) ([]byte, error) { return s.blob(id, sha) }); err != nil {
			return err
		}
	}
	link := filepath.Join(dir, "content", "current")
	if t, err := os.Readlink(link); err != nil || t != fmt.Sprintf("v%d", n) {
		if err := swapLink(link, fmt.Sprintf("v%d", n)); err != nil {
			return err
		}
	}
	if ix, err := s.readIndex(id); err != nil || ix.Version != n {
		return s.writeIndex(r)
	}
	return nil
}

func (s *Store) writeIndex(r *Role) error {
	ix := index{ID: r.ID, Project: r.Project, Name: r.Name, Description: r.Description, Status: r.Status,
		Version: r.Version, ChangedBy: r.ChangedBy, ChangedAt: r.ChangedAt}
	return writeAtomic(filepath.Join(s.roleDir(r.ID), "index.json"), marshal(ix, true), 0o600)
}

// gc removes content/v<n> directories except the newest KeepVersions, the current one and the pinned ones, and
// blobs that no version record names. Version records are history and stay.
func (s *Store) gc(id string, cur int) {
	dir := s.roleDir(id)
	keep := s.pinned(id)
	keep[cur] = true
	ents, _ := os.ReadDir(filepath.Join(dir, "content"))
	var vs []int
	for _, e := range ents {
		if m := contentDirRE.FindStringSubmatch(e.Name()); m != nil {
			v, _ := strconv.Atoi(m[1])
			vs = append(vs, v)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(vs)))
	for i, v := range vs {
		if i < KeepVersions || keep[v] {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, "content", fmt.Sprintf("v%d", v)))
	}
	used := map[string]bool{}
	for _, n := range s.versionNumbers(id) {
		if r, err := s.readVersion(id, n); err == nil {
			for _, sk := range r.Skills {
				used[sk.SHA256] = true
			}
		}
	}
	bents, _ := os.ReadDir(filepath.Join(dir, "blobs"))
	for _, e := range bents {
		if !used[e.Name()] {
			_ = os.Remove(filepath.Join(dir, "blobs", e.Name()))
		}
	}
}

// GC runs the cleanup for one role (Put does it after each commit).
func (s *Store) GC(id string) error {
	if !IsID(id) {
		return errors.New("bad role id")
	}
	defer s.lockRole(id)()
	ix, err := s.readIndex(id)
	if err != nil {
		return err
	}
	s.gc(id, ix.Version)
	return nil
}

func newID() (string, error) {
	const alpha = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	out := make([]byte, 8)
	for i, c := range b {
		out[i] = alpha[int(c)%len(alpha)]
	}
	return "r_" + string(out), nil
}

// allRows lists the stored roles' indexes.
func (s *Store) allIndexes() []*index {
	ents, _ := os.ReadDir(s.opt.Dir)
	var out []*index
	for _, e := range ents {
		if e.IsDir() && IsID(e.Name()) {
			if ix, err := s.readIndex(e.Name()); err == nil {
				out = append(out, ix)
			}
		}
	}
	return out
}

func (s *Store) countDirs() int {
	ents, _ := os.ReadDir(s.opt.Dir)
	n := 0
	for _, e := range ents {
		if e.IsDir() && IsID(e.Name()) {
			n++
		}
	}
	return n
}

// reserve claims project/name for id (when the result is active) and checks it against the stored roles.
func (s *Store) reserve(r *Role) (release func(), err error) {
	if r.Status != "active" {
		return func() {}, nil
	}
	key := r.Project + "/" + r.Name
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	s.mu.Lock()
	holder := s.reserved[key]
	s.mu.Unlock()
	if holder != "" && holder != r.ID {
		return nil, fmt.Errorf("role refused: another role in project %s is called %q", r.Project, r.Name)
	}
	for _, ix := range s.allIndexes() {
		if ix.ID != r.ID && ix.Project == r.Project && ix.Name == r.Name && ix.Status == "active" {
			return nil, fmt.Errorf("role refused: another role in project %s is called %q", r.Project, r.Name)
		}
	}
	s.mu.Lock()
	s.reserved[key] = r.ID
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.reserved, key)
		s.mu.Unlock()
	}, nil
}

// Put validates spec and, if it is sound, commits it as the next version.
func (s *Store) Put(in PutArgs) (*PutResult, error) {
	id := in.ID
	var cur *Role
	spent := newUsed()
	if id != "" {
		if !IsID(id) {
			return nil, errors.New("bad role id")
		}
		defer s.lockRole(id)()
		if err := s.reconcile(id); err != nil {
			if errors.Is(err, errNoRole) {
				return nil, fmt.Errorf("no role %q", id)
			}
			return nil, err
		}
		ix, err := s.readIndex(id)
		if err != nil {
			return nil, err
		}
		if in.BaseVersion != ix.Version {
			return nil, &ConflictError{Base: in.BaseVersion, Current: ix.Version}
		}
		if cur, err = s.readVersion(id, ix.Version); err != nil {
			return nil, err
		}
		if cur.Version >= MaxVersions {
			return nil, fmt.Errorf("role refused: %d versions is the most a role keeps", MaxVersions)
		}
		spent = s.loadUsed(id, cur.Version)
	} else if in.BaseVersion != 0 {
		return nil, errors.New("a new role has base_version 0")
	}
	if in.By != "" && !whoRE.MatchString(in.By) {
		return nil, errors.New("bad \"by\"")
	}

	r, p := s.normalize(in.Spec, cur)
	carry(r, cur, in.By)
	if c := in.Change; c != nil {
		r.Change = &Change{Op: clipText(c.Op, 64), Tier: min(max(c.Tier, 0), 2), Summary: clipText(c.Summary, 200)}
		if approvalRE.MatchString(c.Approval) {
			r.Change.Approval = c.Approval
		}
	}

	// The content of every skill: the blobs sent (re-hashed here) and the ones already stored.
	content := map[string][]byte{}
	if len(in.Blobs) > MaxSkills {
		p.addf("blobs: more than %d", MaxSkills)
	}
	for k, v := range in.Blobs {
		sum := sha256.Sum256([]byte(v))
		switch {
		case len(v) > 32<<10:
			p.addf("blobs: %.12s is over 32 KiB", k)
		case hex.EncodeToString(sum[:]) != k:
			p.addf("blobs: %.12s isn't the sha256 of its content", clipText(k, 64))
		default:
			content[k] = []byte(v)
		}
	}
	sent := map[string]bool{}
	for k := range content {
		sent[k] = true
	}
	used := map[string]bool{}
	for _, sk := range r.Skills {
		if shaRE.MatchString(sk.SHA256) {
			used[sk.SHA256] = true
			if _, ok := content[sk.SHA256]; !ok && id != "" {
				if b, err := s.blob(id, sk.SHA256); err == nil {
					content[sk.SHA256] = b
				}
			}
		}
	}
	for k := range sent {
		if !used[k] {
			p.addf("blobs: %.12s isn't used by any skill", k)
		}
	}
	s.checkSkills(r, content, &p)
	checkApprovals(r, cur, spent, &p)
	if len(p) > 0 {
		return nil, &ValidationError{Reasons: p}
	}

	// Creating: pick the id and take a place under the total limit.
	creating := id == ""
	if creating {
		var err error
		if id, err = s.create(); err != nil {
			return nil, err
		}
		defer s.lockRole(id)()
	}
	ok := false
	defer func() {
		if creating && !ok && len(s.versionNumbers(id)) == 0 {
			_ = os.RemoveAll(s.roleDir(id))
		}
	}()

	now := s.opt.Now().UTC().Format(time.RFC3339)
	r.ID = id
	r.Version = 1
	r.CreatedBy, r.CreatedAt = in.By, now
	if cur != nil {
		r.Version, r.Parent = cur.Version+1, cur.Version
		r.CreatedBy, r.CreatedAt = cur.CreatedBy, cur.CreatedAt
	}
	r.ChangedBy, r.ChangedAt = in.By, now

	release, err := s.reserve(r)
	if err != nil {
		return nil, err
	}
	defer release()

	if err := s.commit(r, content); err != nil {
		return nil, err
	}
	ok = true
	spent.addVersion(cur, r, func(sha string) int { return len(content[sha]) })
	_ = s.saveUsed(id, spent) // a missing or stale used.json is rebuilt from the versions
	s.gc(id, r.Version)
	res := &PutResult{ID: id, Version: r.Version, Skills: []SkillState{}}
	for _, sk := range r.Skills {
		res.Skills = append(res.Skills, SkillState{Name: sk.Name, State: sk.State, Verdict: string(sk.Scan.Verdict)})
	}
	return res, nil
}

// create makes the directory of a new role if the device is under role_max_total.
func (s *Store) create() (string, error) {
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	if s.countDirs() >= s.opt.MaxTotal {
		return "", fmt.Errorf("role refused: this computer already has %d roles (role_max_total)", s.opt.MaxTotal)
	}
	for i := 0; i < 10; i++ {
		id, err := newID()
		if err != nil {
			return "", err
		}
		if err := os.Mkdir(s.roleDir(id), 0o700); err == nil {
			return id, nil
		} else if !os.IsExist(err) {
			return "", err
		}
	}
	return "", errors.New("couldn't make a role id")
}

// commit writes version r in the crash-safe order (see the package comment).
func (s *Store) commit(r *Role, content map[string][]byte) error {
	dir := s.roleDir(r.ID)
	for _, d := range []string{"versions", "blobs", "content", "claude"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return err
		}
	}
	for sha, b := range content {
		p := filepath.Join(dir, "blobs", sha)
		if _, err := os.Lstat(p); err == nil {
			continue
		}
		if err := writeNew(p, b, 0o600); err != nil && err != errExists {
			return err
		}
	}
	if err := s.materialize(r, func(sha string) ([]byte, error) {
		if b, ok := content[sha]; ok {
			return b, nil
		}
		return s.blob(r.ID, sha)
	}); err != nil {
		return err
	}
	if err := writeNew(filepath.Join(dir, "versions", strconv.Itoa(r.Version)+".json"), marshal(r, true), 0o600); err != nil {
		if err == errExists {
			return &ConflictError{Base: r.Parent, Current: r.Version}
		}
		return err
	}
	if err := swapLink(filepath.Join(dir, "content", "current"), fmt.Sprintf("v%d", r.Version)); err != nil {
		return err
	}
	return s.writeIndex(r)
}
