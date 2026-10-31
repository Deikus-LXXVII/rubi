// Package library is the shared skill library (docs/design/agent-roles.md section 8).
//
// Bots submit skills as proposals; the curator Bot admits them. Admitting signs the version with the library key,
// which Rubi Home pins, so a plugin cannot make Home treat an unreviewed skill as a library skill. Versions are
// immutable. Each entry is one file, sealed with XChaCha20-Poly1305 under a key derived from the vault's data key,
// so nothing readable is on disk and nothing can be read while Rubi is locked.
//
// The package does not decide who the curator is: the caller (Rubi's core) checks that, and passes the admitting
// Bot's name.
package library

import (
	"crypto/ed25519"
	"crypto/hmac"
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
	"unicode/utf8"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/Deikus-LXXVII/rubi/internal/library/libsig"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/sdk/skillscan"
)

// Limits.
const (
	MaxVersionBytes    = skillscan.MaxBytes // 32 KiB
	MaxEntries         = 500
	MaxVersionsPerItem = 20 // live versions (proposed, admitted, deprecated) of one entry
	MaxOpenPerBot      = 5  // proposals one Bot may have waiting at once
	maxStoredVersions  = 100
	filesPerEntry      = 4 // entries on disk (whatever their versions' states) per entry allowed live
	MaxTags            = 8
	maxBotName         = 64
)

// State of one version.
type State string

const (
	Proposed   State = "proposed"
	Admitted   State = "admitted"
	Rejected   State = "rejected"
	Deprecated State = "deprecated"
	Withdrawn  State = "withdrawn"
)

var (
	idRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	tagRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	nameR = regexp.MustCompile(`(?m)^name:\s*["']?([a-z0-9][a-z0-9-]{0,47})["']?\s*$`)
)

// Version is one immutable version of an entry. Only State, and what admitting adds, change after it is made.
type Version struct {
	Version      int              `json:"version"`
	SHA256       string           `json:"sha256"`
	Content      string           `json:"content,omitempty"` // empty for a rejected version
	Description  string           `json:"description"`
	Tags         []string         `json:"tags"`
	AuthorBot    string           `json:"author_bot"`
	SubmittedAt  string           `json:"submitted_at"`
	State        State            `json:"state"`
	Scan         skillscan.Result `json:"scan"`
	AdmittedBy   string           `json:"admitted_by,omitempty"`
	AdmittedAt   string           `json:"admitted_at,omitempty"`
	SelfAdmitted bool             `json:"self_admitted,omitempty"`
	Acked        []string         `json:"acked,omitempty"`
	Sig          string           `json:"sig,omitempty"`
	// History is the curator's actions on this version, for the panel.
	History []string `json:"history,omitempty"`
	// Purged marks a withdrawn version whose content was dropped. It stays as a stub, so the version number is
	// never reused and the revocation of its signature keeps being told to Rubi Home.
	Purged bool `json:"purged,omitempty"`
}

func (v *Version) live() bool {
	return v.State == Proposed || v.State == Admitted || v.State == Deprecated
}

// Entry is a skill with its versions.
type Entry struct {
	ID       string     `json:"id"`
	Versions []*Version `json:"versions"`
	// Next is the highest version number ever given (a purge never makes a number come back).
	Next int `json:"next,omitempty"`
	// Counter grows with every save of this entry; an older file put back in its place is refused (index.go).
	Counter uint64 `json:"counter,omitempty"`
}

func (e *Entry) liveCount() int {
	n := 0
	for _, v := range e.Versions {
		if v.live() {
			n++
		}
	}
	return n
}

func (e *Entry) hasAdmitted() bool { return e.LatestAdmitted() > 0 }

func (e *Entry) nextVersion() int {
	n := e.Next
	for _, v := range e.Versions {
		n = max(n, v.Version)
	}
	return n + 1
}

// LatestAdmitted is the highest version that is admitted (0: none).
func (e *Entry) LatestAdmitted() int {
	n := 0
	for _, v := range e.Versions {
		if v.State == Admitted && v.Version > n {
			n = v.Version
		}
	}
	return n
}

func (e *Entry) find(n int) *Version {
	for _, v := range e.Versions {
		if v.Version == n {
			return v
		}
	}
	return nil
}

// Options configure a Library. DEK and Signer are called per operation, so they fail while Rubi is locked.
type Options struct {
	Dir      string
	Instance string
	DEK      func() ([]byte, error)
	Signer   func() (ed25519.PrivateKey, error)
	Now      func() time.Time
	// MaxOpenPerBot is how many proposals one Bot may have waiting (0: MaxOpenPerBot).
	MaxOpenPerBot int
	// MaxEntries is how many skills with a live version there can be (0: MaxEntries).
	MaxEntries int
	// Logf receives notes about files that were skipped (nil: dropped).
	Logf func(format string, args ...any)
}

// Library is the store.
type Library struct {
	opt Options
	mu  sync.Mutex
	idx *counters // highest save counter seen per entry (index.go)
}

func (l *Library) logf(format string, args ...any) {
	if l.opt.Logf != nil {
		l.opt.Logf(format, args...)
	}
}

func (l *Library) maxEntries() int {
	if l.opt.MaxEntries > 0 {
		return l.opt.MaxEntries
	}
	return MaxEntries
}

func (l *Library) maxOpen() int {
	if l.opt.MaxOpenPerBot > 0 {
		return l.opt.MaxOpenPerBot
	}
	return MaxOpenPerBot
}

// Open makes a Library; the directory is created on first write.
func Open(opt Options) *Library {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Library{opt: opt}
}

// ---- sealed files ----

func (l *Library) aead() (interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	NonceSize() int
}, error) {
	dek, err := l.opt.DEK()
	if err != nil {
		return nil, err
	}
	m := hmac.New(sha256.New, dek)
	m.Write([]byte("rubi library store v1"))
	return chacha20poly1305.NewX(m.Sum(nil))
}

func (l *Library) path(id string) string { return filepath.Join(l.opt.Dir, id+".enc") }

func (l *Library) ad(id string) []byte {
	return []byte("rubi-library-v1|" + l.opt.Instance + "|" + id)
}

func (l *Library) load(id string) (*Entry, error) {
	a, err := l.aead()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(l.path(id))
	if err != nil {
		return nil, err
	}
	if len(b) < a.NonceSize() {
		return nil, fmt.Errorf("library entry %s is damaged", id)
	}
	plain, err := a.Open(nil, b[:a.NonceSize()], b[a.NonceSize():], l.ad(id))
	if err != nil {
		return nil, fmt.Errorf("library entry %s is damaged or sealed under another key", id)
	}
	var e Entry
	if err := json.Unmarshal(plain, &e); err != nil || e.ID != id {
		return nil, fmt.Errorf("library entry %s is damaged", id)
	}
	if err := l.checkCounter(&e); err != nil {
		return nil, err
	}
	return &e, nil
}

// loadOrNew is load, but an entry that doesn't exist is a new one that continues the numbering of a removed one.
func (l *Library) loadOrNew(id string) (*Entry, bool, error) {
	e, err := l.load(id)
	if err == nil {
		return e, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	e = &Entry{ID: id}
	c, ierr := l.index()
	if ierr != nil {
		return nil, false, ierr
	}
	e.Next, e.Counter = c.m[id].Next, c.m[id].Counter
	return e, true, nil
}

func (l *Library) save(e *Entry) error {
	a, err := l.aead()
	if err != nil {
		return err
	}
	c, err := l.index()
	if err != nil {
		return err
	}
	rec := c.m[e.ID]
	e.Counter = max(e.Counter, rec.Counter) + 1
	e.Next = max(e.Next, rec.Next, e.nextVersion()-1)
	plain, err := json.Marshal(e)
	if err != nil {
		return err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	if err := os.MkdirAll(l.opt.Dir, 0o700); err != nil {
		return err
	}
	if err := vault.WriteFileAtomic(l.path(e.ID), append(nonce, a.Seal(nil, nonce, plain, l.ad(e.ID))...), 0o600); err != nil {
		return err
	}
	// The file first: a crash in between leaves a file with a counter above the index's, which is accepted.
	c.m[e.ID] = idxRec{Counter: e.Counter, Next: e.Next}
	if err := l.saveIndex(c); err != nil {
		l.logf("library: the index couldn't be written: %v", err)
	}
	return nil
}

func (l *Library) ids() []string {
	files, _ := os.ReadDir(l.opt.Dir)
	var out []string
	for _, f := range files {
		if n, ok := strings.CutSuffix(f.Name(), ".enc"); ok && idRE.MatchString(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// each calls f with every entry that can be read; a damaged one is logged and skipped, so one bad file doesn't
// take the whole library down.
func (l *Library) each(f func(*Entry)) error {
	if _, err := l.aead(); err != nil { // locked: nothing can be read, and that is not a damaged file
		return err
	}
	for _, id := range l.ids() {
		e, err := l.load(id)
		if err != nil {
			l.logf("library: skipping %s: %v", id, err)
			continue
		}
		f(e)
	}
	return nil
}

// ---- submit ----

// Submission is what a Bot proposes.
type Submission struct {
	ID          string // empty: the skill's own name
	Description string
	Tags        []string
	Content     string
	Author      string // the verified Bot
	// Curator says the author is the library's curator (the caller, Rubi's core, decides): only the curator adds
	// versions to a skill that already has an admitted version, and the per-Bot limit on open proposals is not its.
	Curator bool
}

// Proposal is the outcome of a submission.
type Proposal struct {
	ProposalID string           `json:"proposal_id"`
	ID         string           `json:"id"`
	Version    int              `json:"version"`
	State      State            `json:"state"`
	Scan       skillscan.Result `json:"scan"`
}

// ProposalID names one version: "id@3".
func ProposalID(id string, version int) string { return id + "@" + strconv.Itoa(version) }

func parseProposal(p string) (string, int, error) {
	i := strings.LastIndexByte(p, '@')
	if i < 1 {
		return "", 0, errors.New("a proposal id looks like id@3")
	}
	n, err := strconv.Atoi(p[i+1:])
	if err != nil || n < 1 || !idRE.MatchString(p[:i]) {
		return "", 0, errors.New("a proposal id looks like id@3")
	}
	return p[:i], n, nil
}

func checkText(desc string, tags []string) ([]string, error) {
	if strings.TrimSpace(desc) == "" || utf8.RuneCountInString(desc) > skillscan.MaxDescription {
		return nil, fmt.Errorf("the description is required and at most %d characters", skillscan.MaxDescription)
	}
	if len(tags) > MaxTags {
		return nil, fmt.Errorf("at most %d tags", MaxTags)
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if !tagRE.MatchString(t) {
			return nil, fmt.Errorf("tag %q: lower-case letters, digits and hyphens, up to 32 characters", clip(t, 20))
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	// The description and tags are shown to people and Bots, so they get the scan a skill's text gets.
	for _, h := range skillscan.ScanText(desc+"\n"+strings.Join(out, " "), nil) {
		return nil, fmt.Errorf("the description or tags trip rule %s", h.ID)
	}
	return out, nil
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

func validBot(name string) error {
	if name == "" || len(name) > maxBotName {
		return errors.New("a Bot name is required (at most 64 bytes)")
	}
	return nil
}

// Submit records a proposal. A scan reject is final: the version is kept as rejected (without its content) and
// can never be admitted.
func (l *Library) Submit(in Submission) (Proposal, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.submitLocked(in)
}

func (l *Library) submitLocked(in Submission) (Proposal, error) {
	if err := validBot(in.Author); err != nil {
		return Proposal{}, err
	}
	if len(in.Content) > 2*MaxVersionBytes {
		return Proposal{}, fmt.Errorf("a skill is at most %d KiB", MaxVersionBytes/1024)
	}
	id := in.ID
	if id == "" {
		if m := nameR.FindStringSubmatch(in.Content); m != nil {
			id = m[1]
		}
	}
	if !idRE.MatchString(id) {
		return Proposal{}, errors.New("pass id: lower-case letters, digits and hyphens, up to 48 characters (it is also the skill's name:)")
	}
	tags, err := checkText(in.Description, in.Tags)
	if err != nil {
		return Proposal{}, err
	}
	e, fresh, err := l.loadOrNew(id)
	if err != nil {
		return Proposal{}, err
	}
	if fresh {
		live, total := 0, 0
		if err := l.each(func(o *Entry) {
			total++
			if o.liveCount() > 0 {
				live++
			}
		}); err != nil {
			return Proposal{}, err
		}
		// Only skills with a live version count against the limit; the rest wait for the curator's purge.
		if live >= l.maxEntries() || total >= filesPerEntry*l.maxEntries() {
			return Proposal{}, fmt.Errorf("the library holds at most %d skills", l.maxEntries())
		}
	} else if e.hasAdmitted() && !in.Curator {
		return Proposal{}, fmt.Errorf("%s is admitted already: only the curator adds versions to it (ask the curator to publish your change)", id)
	}
	if n := e.liveCount(); n >= MaxVersionsPerItem {
		return Proposal{}, fmt.Errorf("%s already has %d live versions, the most there can be (the curator can withdraw and purge old ones)", id, n)
	}
	if len(e.Versions) >= maxStoredVersions {
		return Proposal{}, fmt.Errorf("%s holds %d versions; the curator must purge rejected or withdrawn ones first", id, len(e.Versions))
	}
	if !in.Curator {
		open := 0
		if err := l.each(func(o *Entry) {
			for _, v := range o.Versions {
				if v.State == Proposed && strings.EqualFold(v.AuthorBot, in.Author) {
					open++
				}
			}
		}); err != nil {
			return Proposal{}, err
		}
		if open >= l.maxOpen() {
			return Proposal{}, fmt.Errorf("you have %d proposals waiting already, the most there can be; wait for the curator", open)
		}
	}
	sum := sha256.Sum256([]byte(in.Content))
	now := l.opt.Now().UTC().Format(time.RFC3339)
	scan := skillscan.Scan(id, []byte(in.Content), nil)
	v := &Version{Version: e.nextVersion(), SHA256: hex.EncodeToString(sum[:]), Content: in.Content,
		Description: in.Description, Tags: tags, AuthorBot: in.Author, SubmittedAt: now, State: Proposed, Scan: scan,
		History: []string{now + " submitted by " + in.Author}}
	if scan.Verdict == skillscan.Reject {
		v.State, v.Content = Rejected, ""
		v.History = append(v.History, now+" rejected by the scan")
	}
	e.Versions = append(e.Versions, v)
	if err := l.save(e); err != nil {
		return Proposal{}, err
	}
	return Proposal{ProposalID: ProposalID(id, v.Version), ID: id, Version: v.Version, State: v.State, Scan: scan}, nil
}

// ---- curator actions ----

func flaggedIDs(res skillscan.Result) []string {
	seen := map[string]bool{}
	var ids []string
	for _, h := range res.Rules {
		if h.Severity == skillscan.Flag && !seen[h.ID] {
			seen[h.ID] = true
			ids = append(ids, h.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// AdmitResult says what was done.
type AdmitResult struct {
	ProposalID   string   `json:"proposal_id"`
	ID           string   `json:"id"`
	Version      int      `json:"version"`
	State        State    `json:"state"`
	SelfAdmitted bool     `json:"self_admitted"`
	Acked        []string `json:"acked,omitempty"`
}

// Admit admits a proposed version (or re-admits a deprecated or withdrawn one) on behalf of the curator by. The
// scan runs again and must pass; every flag must be acknowledged by rule id. A curator admitting a version it
// wrote itself is marked self-admitted.
func (l *Library) Admit(proposalID, by string, ack []string) (AdmitResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.admitLocked(proposalID, by, ack)
}

func (l *Library) admitLocked(proposalID, by string, ack []string) (AdmitResult, error) {
	if err := validBot(by); err != nil {
		return AdmitResult{}, err
	}
	id, n, err := parseProposal(proposalID)
	if err != nil {
		return AdmitResult{}, err
	}
	e, err := l.load(id)
	if err != nil {
		return AdmitResult{}, noSuch(err, proposalID)
	}
	v := e.find(n)
	if v == nil {
		return AdmitResult{}, fmt.Errorf("no proposal %s", proposalID)
	}
	if v.Purged {
		return AdmitResult{}, fmt.Errorf("%s was purged; submit it again", proposalID)
	}
	if v.State == Withdrawn && e.liveCount() >= MaxVersionsPerItem {
		return AdmitResult{}, fmt.Errorf("%s already has %d live versions; purge or withdraw one first", id, e.liveCount())
	}
	switch v.State {
	case Proposed, Deprecated, Withdrawn:
	case Rejected:
		return AdmitResult{}, fmt.Errorf("%s was rejected by the scan; that is final (submit a corrected version)", proposalID)
	default:
		return AdmitResult{}, fmt.Errorf("%s is already %s", proposalID, v.State)
	}
	// The checks run again now: the scanner may have changed since the submission.
	scan := skillscan.Scan(id, []byte(v.Content), nil)
	if scan.Verdict == skillscan.Reject {
		return AdmitResult{}, fmt.Errorf("%s no longer passes the scan: it can't be admitted", proposalID)
	}
	have := map[string]bool{}
	for _, a := range ack {
		have[a] = true
	}
	var missing []string
	flags := flaggedIDs(scan)
	for _, f := range flags {
		if !have[f] {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return AdmitResult{}, fmt.Errorf("the scan flagged %s: acknowledge each rule id in ack to admit", strings.Join(flags, ", "))
	}
	priv, err := l.opt.Signer()
	if err != nil {
		return AdmitResult{}, err
	}
	now := l.opt.Now().UTC().Format(time.RFC3339)
	self := strings.EqualFold(by, v.AuthorBot)
	tok, err := libsig.Sign(priv, libsig.Claims{ID: id, Version: n, SHA256: v.SHA256, AdmittedBy: by, AdmittedAt: now,
		ScanVerdict: string(scan.Verdict), ScannerVersion: scan.ScannerVersion, SelfAdmitted: self, Acked: flags})
	if err != nil {
		return AdmitResult{}, err
	}
	v.State, v.Scan, v.AdmittedBy, v.AdmittedAt, v.SelfAdmitted, v.Acked, v.Sig = Admitted, scan, by, now, self, flags, tok
	note := now + " admitted by " + by
	if self {
		note += " (self-admitted by curator)"
	}
	v.History = append(v.History, note)
	if err := l.save(e); err != nil {
		return AdmitResult{}, err
	}
	return AdmitResult{ProposalID: proposalID, ID: id, Version: n, State: Admitted, SelfAdmitted: self, Acked: flags}, nil
}

func noSuch(err error, what string) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no such skill or proposal: %s", what)
	}
	return err
}

// Publish is a curator's new version in one step: submit as the curator, then admit.
func (l *Library) Publish(in Submission, ack []string) (AdmitResult, Proposal, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	in.Curator = true // only the curator publishes
	p, err := l.submitLocked(in)
	if err != nil {
		return AdmitResult{}, p, err
	}
	if p.State == Rejected {
		return AdmitResult{}, p, errors.New("the scan rejected this skill; that is final")
	}
	res, err := l.admitLocked(p.ProposalID, in.Author, ack)
	return res, p, err
}

// SetState moves versions to deprecated or withdrawn. version 0 means every version that is admitted (deprecate)
// or every version that is not already withdrawn (withdraw). It returns the versions changed.
func (l *Library) SetState(id string, version int, to State, by string) ([]int, error) {
	if to != Deprecated && to != Withdrawn {
		return nil, errors.New("only deprecated or withdrawn")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := validBot(by); err != nil {
		return nil, err
	}
	e, err := l.load(id)
	if err != nil {
		return nil, noSuch(err, id)
	}
	now := l.opt.Now().UTC().Format(time.RFC3339)
	var changed []int
	for _, v := range e.Versions {
		if version != 0 && v.Version != version {
			continue
		}
		ok := false
		switch to {
		case Deprecated:
			ok = v.State == Admitted
		case Withdrawn:
			ok = v.State == Admitted || v.State == Deprecated || v.State == Proposed
		}
		if !ok {
			continue
		}
		v.State = to
		v.History = append(v.History, now+" "+string(to)+" by "+by)
		changed = append(changed, v.Version)
	}
	if len(changed) == 0 {
		return nil, fmt.Errorf("nothing to %s in %s", strings.TrimSuffix(string(to), "d"), id)
	}
	return changed, l.save(e)
}

// Purge drops versions nobody uses any more: rejected and proposed ones go entirely, a withdrawn one loses its
// content and stays as a stub (so its number is not reused and its revocation stays in force). version 0 means
// every such version of the skill. It returns the versions purged. An entry left with no versions is removed.
func (l *Library) Purge(id string, version int, by string) ([]int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := validBot(by); err != nil {
		return nil, err
	}
	if !idRE.MatchString(id) {
		return nil, fmt.Errorf("no such skill: %s", clip(id, 20))
	}
	e, err := l.load(id)
	if err != nil {
		return nil, noSuch(err, id)
	}
	var purged []int
	kept := e.Versions[:0:0]
	for _, v := range e.Versions {
		pick := version == 0 || v.Version == version
		switch {
		case pick && (v.State == Rejected || v.State == Proposed):
			purged = append(purged, v.Version)
			continue
		case pick && v.State == Withdrawn && !v.Purged:
			v.Content, v.Purged = "", true
			v.History = append(v.History, l.opt.Now().UTC().Format(time.RFC3339)+" purged by "+by)
			purged = append(purged, v.Version)
		}
		kept = append(kept, v)
	}
	if len(purged) == 0 {
		return nil, fmt.Errorf("nothing to purge in %s (only rejected, withdrawn and unadmitted versions can be)", id)
	}
	e.Next = max(e.Next, e.nextVersion()-1)
	e.Versions = kept
	if len(kept) == 0 {
		return purged, l.remove(e)
	}
	return purged, l.save(e)
}

// Revoked lists the withdrawn versions as "id@version", sorted: what Rubi Home must refuse to use.
func (l *Library) Revoked() ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []string{}
	err := l.each(func(e *Entry) {
		for _, v := range e.Versions {
			if v.State == Withdrawn {
				out = append(out, ProposalID(e.ID, v.Version))
			}
		}
	})
	sort.Strings(out)
	return out, err
}

// ---- reading ----

// Info is a version as shown to a caller.
type Info struct {
	ID             string   `json:"id"`
	Version        int      `json:"version"`
	State          State    `json:"state"`
	Description    string   `json:"description"`
	Tags           []string `json:"tags"`
	SHA256         string   `json:"sha256"`
	Content        string   `json:"content,omitempty"`
	AuthorBot      string   `json:"author_bot"`
	SubmittedAt    string   `json:"submitted_at"`
	AdmittedBy     string   `json:"admitted_by,omitempty"`
	AdmittedAt     string   `json:"admitted_at,omitempty"`
	SelfAdmitted   bool     `json:"self_admitted,omitempty"`
	ScanVerdict    string   `json:"scan_verdict"`
	ScanRules      []string `json:"scan_rules,omitempty"`
	ScannerVersion string   `json:"scanner_version"`
	Signature      string   `json:"signature,omitempty"`
	LatestAdmitted int      `json:"latest_admitted"`
	History        []string `json:"history,omitempty"`
}

func info(e *Entry, v *Version, withContent, curator bool) Info {
	in := Info{ID: e.ID, Version: v.Version, State: v.State, Description: v.Description, Tags: v.Tags, SHA256: v.SHA256,
		AuthorBot: v.AuthorBot, SubmittedAt: v.SubmittedAt, AdmittedBy: v.AdmittedBy, AdmittedAt: v.AdmittedAt,
		SelfAdmitted: v.SelfAdmitted, ScanVerdict: string(v.Scan.Verdict), ScannerVersion: v.Scan.ScannerVersion,
		LatestAdmitted: e.LatestAdmitted()}
	for _, h := range v.Scan.Rules {
		in.ScanRules = append(in.ScanRules, h.ID)
	}
	if v.State == Admitted || v.State == Deprecated {
		in.Signature = v.Sig
	}
	if withContent {
		in.Content = v.Content
	}
	if curator {
		in.History = v.History
	}
	return in
}

func visible(v *Version, curator bool) bool {
	return curator || v.State == Admitted || v.State == Deprecated
}

// Summary is one line of the list.
type Summary struct {
	ID             string   `json:"id"`
	Description    string   `json:"description"`
	Tags           []string `json:"tags"`
	LatestAdmitted int      `json:"latest_admitted"`
	Versions       []VState `json:"versions"`
}

// VState is a version number with its state.
type VState struct {
	Version int   `json:"version"`
	State   State `json:"state"`
}

// List returns the entries that match query (id, description or tag, case-insensitive) and tag. Everyone sees the
// admitted and deprecated versions; the curator sees every version.
func (l *Library) List(query, tag string, curator bool) ([]Summary, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	query, tag = strings.ToLower(strings.TrimSpace(query)), strings.ToLower(strings.TrimSpace(tag))
	out := []Summary{}
	err := l.each(func(e *Entry) {
		id := e.ID
		s := Summary{ID: id, LatestAdmitted: e.LatestAdmitted(), Versions: []VState{}}
		var shown *Version
		for _, v := range e.Versions {
			if visible(v, curator) {
				s.Versions = append(s.Versions, VState{v.Version, v.State})
				shown = v
			}
		}
		if shown == nil {
			return
		}
		if n := e.LatestAdmitted(); n > 0 {
			shown = e.find(n)
		}
		s.Description, s.Tags = shown.Description, shown.Tags
		if tag != "" && !has(s.Tags, tag) {
			return
		}
		if query != "" && !strings.Contains(strings.ToLower(id+" "+s.Description+" "+strings.Join(s.Tags, " ")), query) {
			return
		}
		out = append(out, s)
	})
	return out, err
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Get returns one version with its content. version 0 means the latest admitted. Without curator, only admitted
// and deprecated versions exist; for anything else the answer is the same "no such version".
func (l *Library) Get(id string, version int, curator bool) (Info, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	none := fmt.Errorf("no admitted skill %s%s", id, versionSuffix(version))
	if !idRE.MatchString(id) {
		return Info{}, none
	}
	e, err := l.load(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Info{}, none
		}
		return Info{}, err
	}
	if version == 0 {
		if version = e.LatestAdmitted(); version == 0 {
			if curator {
				return Info{}, fmt.Errorf("%s has no admitted version", id)
			}
			return Info{}, none
		}
	}
	v := e.find(version)
	if v == nil || !visible(v, curator) {
		return Info{}, none
	}
	return info(e, v, true, curator), nil
}

// GetAdmitted is Get for a plugin building a role: the version must be admitted or deprecated (a deprecated one is
// only for roles that already use it; the caller sees State).
func (l *Library) GetAdmitted(id string, version int) (Info, error) { return l.Get(id, version, false) }

func versionSuffix(n int) string {
	if n == 0 {
		return ""
	}
	return "@" + strconv.Itoa(n)
}

// ---- the owner's view (panel) ----

// PanelVersion is a version as the panel shows it: Info and the scan's hits. It never carries content; the
// panel asks for one version's content with Get. Every text in it came from a Bot and is drawn as text only.
type PanelVersion struct {
	Info
	Hits     []skillscan.Hit `json:"scan_hits,omitempty"`
	Stripped []string        `json:"scan_stripped,omitempty"`
}

func panelVersion(e *Entry, v *Version) PanelVersion {
	pv := PanelVersion{Info: info(e, v, false, true), Hits: v.Scan.Rules, Stripped: v.Scan.Stripped}
	pv.Info.Content = ""
	return pv
}

// Entry returns every version of one skill (all states) for the owner, without content.
func (l *Library) Entry(id string) ([]PanelVersion, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !idRE.MatchString(id) {
		return nil, fmt.Errorf("no skill %s", id)
	}
	e, err := l.load(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no skill %s", id)
		}
		return nil, err
	}
	out := make([]PanelVersion, 0, len(e.Versions))
	for _, v := range e.Versions {
		out = append(out, panelVersion(e, v))
	}
	return out, nil
}

// Queue returns the proposals waiting for the curator, oldest first by skill id, without content.
func (l *Library) Queue() ([]PanelVersion, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []PanelVersion{}
	err := l.each(func(e *Entry) {
		for _, v := range e.Versions {
			if v.State == Proposed {
				out = append(out, panelVersion(e, v))
			}
		}
	})
	return out, err
}
