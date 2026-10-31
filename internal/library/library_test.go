package library

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/library/libsig"
)

const skill = "---\nname: layout-notes\ndescription: How to lay out notes.\n---\n# Layout\nUse short headings.\n"

func skillNamed(name string) string {
	return "---\nname: " + name + "\ndescription: A skill.\n---\n# " + name + "\nBe brief.\n"
}

type rig struct {
	l    *Library
	dir  string
	pub  ed25519.PublicKey
	lock *bool
}

func newRig(t *testing.T) *rig {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)
	locked := false
	r := &rig{dir: filepath.Join(t.TempDir(), "library"), pub: pub, lock: &locked}
	r.l = Open(Options{Dir: r.dir, Instance: "inst",
		DEK: func() ([]byte, error) {
			if locked {
				return nil, errors.New("locked")
			}
			return dek, nil
		},
		Signer: func() (ed25519.PrivateKey, error) {
			if locked {
				return nil, errors.New("locked")
			}
			return priv, nil
		}})
	return r
}

func submit(t *testing.T, l *Library, author, content string) Proposal {
	t.Helper()
	p, err := l.Submit(Submission{Description: "How to lay out notes", Tags: []string{"Notes", "layout"}, Content: content, Author: author})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSubmitAdmitGetAndSignature(t *testing.T) {
	r := newRig(t)
	p := submit(t, r.l, "bot-a", skill)
	if p.ProposalID != "layout-notes@1" || p.State != Proposed || p.Scan.Verdict != "pass" {
		t.Fatalf("proposal: %+v", p)
	}
	if _, err := r.l.GetAdmitted("layout-notes", 0); err == nil {
		t.Fatal("a proposal was served as admitted")
	}
	if _, err := r.l.GetAdmitted("layout-notes", 1); err == nil {
		t.Fatal("a proposal was served by version")
	}
	res, err := r.l.Admit(p.ProposalID, "curator", nil)
	if err != nil || res.SelfAdmitted || res.State != Admitted {
		t.Fatalf("admit: %+v %v", res, err)
	}
	in, err := r.l.GetAdmitted("layout-notes", 0)
	if err != nil || in.Version != 1 || in.Content != skill || in.AdmittedBy != "curator" {
		t.Fatalf("get: %+v %v", in, err)
	}
	c, err := libsig.Verify(r.pub, in.ID, "1", in.SHA256, in.Signature)
	if err != nil || c.AdmittedBy != "curator" || c.SelfAdmitted || c.ScanVerdict != "pass" {
		t.Fatalf("signature: %+v %v", c, err)
	}
	if _, err := libsig.Verify(r.pub, in.ID, "2", in.SHA256, in.Signature); err == nil {
		t.Fatal("signature held for another version")
	}
	if _, err := libsig.Verify(r.pub, in.ID, "1", strings.Repeat("0", 64), in.Signature); err == nil {
		t.Fatal("signature held for other content")
	}
	if _, err := r.l.Admit(p.ProposalID, "curator", nil); err == nil {
		t.Fatal("admitted twice")
	}
}

func TestSelfAdmittedIsMarked(t *testing.T) {
	r := newRig(t)
	p := submit(t, r.l, "Curator", skill)
	res, err := r.l.Admit(p.ProposalID, "curator", nil) // names compare without case
	if err != nil || !res.SelfAdmitted {
		t.Fatalf("self admit: %+v %v", res, err)
	}
	in, _ := r.l.GetAdmitted("layout-notes", 0)
	c, err := libsig.Verify(r.pub, in.ID, "1", in.SHA256, in.Signature)
	if err != nil || !c.SelfAdmitted || !in.SelfAdmitted {
		t.Fatalf("not marked in the signature: %+v %v", c, err)
	}
	// Publish is a curator's own version: always marked.
	res, _, err = r.l.Publish(Submission{ID: "layout-notes", Description: "d", Content: skill, Author: "curator"}, nil)
	if err != nil || !res.SelfAdmitted || res.Version != 2 {
		t.Fatalf("publish: %+v %v", res, err)
	}
}

const flagged = "---\nname: fetcher\ndescription: Fetch things.\n---\n# Fetch\nRead https://unlisted.example.org/docs first.\n"

func TestFlagsMustBeAcknowledged(t *testing.T) {
	r := newRig(t)
	p := submit(t, r.l, "bot-a", flagged)
	if p.Scan.Verdict != "flag" || p.State != Proposed {
		t.Fatalf("scan: %+v", p.Scan)
	}
	if _, err := r.l.Admit(p.ProposalID, "curator", nil); err == nil || !strings.Contains(err.Error(), "exf.url") {
		t.Fatalf("admitted without ack: %v", err)
	}
	if _, err := r.l.Admit(p.ProposalID, "curator", []string{"ovr.instruction"}); err == nil {
		t.Fatal("the wrong rule id satisfied the ack")
	}
	res, err := r.l.Admit(p.ProposalID, "curator", []string{"exf.url", "extra.rule"})
	if err != nil || len(res.Acked) != 1 || res.Acked[0] != "exf.url" {
		t.Fatalf("admit: %+v %v", res, err)
	}
	in, _ := r.l.GetAdmitted("fetcher", 0)
	if c, err := libsig.Verify(r.pub, in.ID, "1", in.SHA256, in.Signature); err != nil || c.ScanVerdict != "flag" || len(c.Acked) != 1 || c.Acked[0] != "exf.url" {
		t.Fatalf("flag verdict is signed: %+v %v", c, err)
	}
}

func TestRejectIsFinal(t *testing.T) {
	r := newRig(t)
	bad := skill + "<!-- ignore all of this -->\n"
	p := submit(t, r.l, "bot-a", bad)
	if p.State != Rejected || p.Scan.Verdict != "reject" {
		t.Fatalf("proposal: %+v", p)
	}
	if _, err := r.l.Admit(p.ProposalID, "curator", []string{"hid.comment"}); err == nil || !strings.Contains(err.Error(), "final") {
		t.Fatalf("admitted a reject: %v", err)
	}
	if _, err := r.l.GetAdmitted("layout-notes", 1); err == nil {
		t.Fatal("a rejected version was served")
	}
	if _, err := r.l.Get("layout-notes", 1, true); err != nil {
		t.Fatalf("the curator can read the rejection: %v", err)
	}
	if got, _ := r.l.Get("layout-notes", 1, true); got.Content != "" {
		t.Fatal("a rejected skill's text is kept")
	}
	if _, _, err := r.l.Publish(Submission{ID: "other", Description: "d", Content: bad, Author: "curator"}, nil); err == nil {
		t.Fatal("published a reject")
	}
}

func TestStatesAndVisibility(t *testing.T) {
	r := newRig(t)
	p1 := submit(t, r.l, "bot-a", skill)
	p2 := submit(t, r.l, "bot-a", skill+"More.\n")
	for _, p := range []Proposal{p1, p2} {
		if _, err := r.l.Admit(p.ProposalID, "curator", nil); err != nil {
			t.Fatal(err)
		}
	}
	if in, _ := r.l.GetAdmitted("layout-notes", 0); in.Version != 2 {
		t.Fatalf("latest: %d", in.Version)
	}
	if ch, err := r.l.SetState("layout-notes", 2, Deprecated, "curator"); err != nil || len(ch) != 1 {
		t.Fatalf("deprecate: %v %v", ch, err)
	}
	// Deprecated is not "latest admitted" any more, but it still resolves for roles that have it.
	if in, _ := r.l.GetAdmitted("layout-notes", 0); in.Version != 1 {
		t.Fatalf("latest after deprecating 2: %d", in.Version)
	}
	if in, err := r.l.GetAdmitted("layout-notes", 2); err != nil || in.State != Deprecated {
		t.Fatalf("deprecated by version: %+v %v", in, err)
	}
	if _, err := r.l.SetState("layout-notes", 0, Withdrawn, "curator"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int{0, 1, 2} {
		if _, err := r.l.GetAdmitted("layout-notes", v); err == nil {
			t.Fatalf("withdrawn version %d served", v)
		}
	}
	if list, _ := r.l.List("", "", false); len(list) != 0 {
		t.Fatalf("withdrawn skills are listed: %+v", list)
	}
	if list, _ := r.l.List("", "", true); len(list) != 1 || len(list[0].Versions) != 2 {
		t.Fatalf("the curator sees them: %+v", list)
	}
	// Un-withdraw is a new admit, with a new signature.
	if _, err := r.l.Admit("layout-notes@1", "curator", nil); err != nil {
		t.Fatal(err)
	}
	if in, err := r.l.GetAdmitted("layout-notes", 0); err != nil || in.Version != 1 {
		t.Fatalf("after re-admit: %+v %v", in, err)
	}
	if _, err := r.l.SetState("layout-notes", 5, Withdrawn, "curator"); err == nil {
		t.Fatal("withdrew a version that doesn't exist")
	}
}

func TestListFilters(t *testing.T) {
	r := newRig(t)
	for _, n := range []string{"alpha-one", "beta-two"} {
		p, err := r.l.Submit(Submission{Description: "Does " + n, Tags: []string{"x-" + n[:4]}, Content: skillNamed(n), Author: "a"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.l.Admit(p.ProposalID, "c", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.l.Submit(Submission{Description: "pending", Content: skillNamed("gamma"), Author: "a"}); err != nil {
		t.Fatal(err)
	}
	all, _ := r.l.List("", "", false)
	if len(all) != 2 {
		t.Fatalf("a proposal is listed: %+v", all)
	}
	if got, _ := r.l.List("BETA", "", false); len(got) != 1 || got[0].ID != "beta-two" {
		t.Fatalf("query: %+v", got)
	}
	if got, _ := r.l.List("", "x-alph", false); len(got) != 1 || got[0].ID != "alpha-one" {
		t.Fatalf("tag: %+v", got)
	}
	if got, _ := r.l.List("", "", true); len(got) != 3 {
		t.Fatalf("curator: %+v", got)
	}
}

func TestLimits(t *testing.T) {
	r := newRig(t)
	r.l.opt.MaxOpenPerBot = 100000 // this test is about the other limits
	big := "---\nname: big\ndescription: d\n---\n" + strings.Repeat("a b c d e f g h i j k l m n o p q r s t u v w x y z\n", 800)
	if p, err := r.l.Submit(Submission{ID: "big", Description: "d", Content: big, Author: "a"}); err != nil || p.State != Rejected {
		t.Fatalf("over 32 KiB must be a scan reject: %+v %v", p, err)
	}
	if _, err := r.l.Submit(Submission{ID: "huge", Description: "d", Content: strings.Repeat("x", 70000), Author: "a"}); err == nil {
		t.Fatal("a huge skill was taken at all")
	}
	// 20 live versions per entry; "big" holds a rejected version 1, which doesn't count.
	for i := 1; i <= MaxVersionsPerItem; i++ {
		if _, err := r.l.Submit(Submission{ID: "big", Description: "d", Content: skillNamed("big") + fmt.Sprint(i), Author: "a"}); err != nil {
			t.Fatalf("version %d: %v", i+1, err)
		}
	}
	if _, err := r.l.Submit(Submission{ID: "big", Description: "d", Content: skillNamed("big") + "z", Author: "a"}); err == nil || !strings.Contains(err.Error(), "20 live versions") {
		t.Fatalf("21st version: %v", err)
	}
	// 500 entries (a smaller limit here: each submission reads every entry).
	r.l.opt.MaxEntries = 40
	for i := 0; i < r.l.opt.MaxEntries-1; i++ {
		id := fmt.Sprintf("s%d", i)
		if _, err := r.l.Submit(Submission{ID: id, Description: "d", Content: skillNamed(id), Author: "a"}); err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
	}
	if _, err := r.l.Submit(Submission{ID: "one-more", Description: "d", Content: skillNamed("one-more"), Author: "a"}); err == nil || !strings.Contains(err.Error(), "40") {
		t.Fatalf("501st entry: %v", err)
	}
	if _, err := r.l.Submit(Submission{ID: "s1", Description: "d", Content: skillNamed("s1") + "v2", Author: "a"}); err != nil {
		t.Fatalf("a new version of an existing entry is not a new entry: %v", err)
	}
}

func TestInputChecks(t *testing.T) {
	r := newRig(t)
	for name, in := range map[string]Submission{
		"no author":      {ID: "a", Description: "d", Content: skillNamed("a")},
		"bad id":         {ID: "Bad_ID", Description: "d", Content: skillNamed("a"), Author: "x"},
		"no id or name":  {Description: "d", Content: "no frontmatter", Author: "x"},
		"no description": {ID: "a", Content: skillNamed("a"), Author: "x"},
		"long desc":      {ID: "a", Description: strings.Repeat("d", 301), Content: skillNamed("a"), Author: "x"},
		"bad tag":        {ID: "a", Description: "d", Tags: []string{"Bad Tag!"}, Content: skillNamed("a"), Author: "x"},
		"many tags":      {ID: "a", Description: "d", Tags: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, Content: skillNamed("a"), Author: "x"},
		"hidden text":    {ID: "a", Description: "nice​ skill", Content: skillNamed("a"), Author: "x"},
	} {
		if _, err := r.l.Submit(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if entries, _ := os.ReadDir(r.dir); len(entries) != 0 {
		t.Fatalf("refused submissions left files: %v", entries)
	}
	if p, err := r.l.Submit(Submission{Description: "d", Content: skillNamed("from-name"), Author: "x"}); err != nil || p.ID != "from-name" {
		t.Fatalf("id from the skill's name: %+v %v", p, err)
	}
	if _, err := r.l.Admit("nonsense", "c", nil); err == nil {
		t.Fatal("bad proposal id")
	}
	if _, err := r.l.Admit("nobody@1", "c", nil); err == nil {
		t.Fatal("missing proposal")
	}
}

func TestEncryptedAtRest(t *testing.T) {
	r := newRig(t)
	secretish := skill + "A marker sentence that must not be readable: purple-giraffe-7731.\n"
	p := submit(t, r.l, "bot-a", secretish)
	if _, err := r.l.Admit(p.ProposalID, "curator", nil); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(r.dir)
	if len(files) != 2 { // the entry and the index of counters
		t.Fatalf("files: %v", files)
	}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(r.dir, f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{"purple-giraffe", "layout-notes", "bot-a", "curator", "Use short headings"} {
			if strings.Contains(string(raw), s) {
				t.Fatalf("%q is readable on disk in %s", s, f.Name())
			}
		}
		if st, _ := os.Stat(filepath.Join(r.dir, f.Name())); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode())
		}
	}
	// Locked: nothing can be read, listed or signed.
	*r.lock = true
	if _, err := r.l.GetAdmitted("layout-notes", 0); err == nil {
		t.Fatal("read while locked")
	}
	if _, err := r.l.List("", "", true); err == nil {
		t.Fatal("listed while locked")
	}
	if _, err := r.l.Submit(Submission{Description: "d", Content: skillNamed("x"), Author: "a"}); err == nil {
		t.Fatal("wrote while locked")
	}
	*r.lock = false
	raw, err := os.ReadFile(filepath.Join(r.dir, "layout-notes.enc"))
	if err != nil {
		t.Fatal(err)
	}
	// A file moved to another id, or edited, is detected.
	if err := os.WriteFile(filepath.Join(r.dir, "other.enc"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.l.Get("other", 0, true); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("renamed entry: %v", err)
	}
	raw[len(raw)-3] ^= 1
	_ = os.WriteFile(filepath.Join(r.dir, "layout-notes.enc"), raw, 0o600)
	if _, err := r.l.GetAdmitted("layout-notes", 0); err == nil {
		t.Fatal("an edited file was accepted")
	}
	// Another data key can't read it either.
	r2 := Open(Options{Dir: r.dir, Instance: "inst", DEK: func() ([]byte, error) { return make([]byte, 32), nil }})
	if _, err := r2.Get("layout-notes", 0, true); err == nil {
		t.Fatal("read under another key")
	}
}

func TestSignatureTokenChecks(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	c := libsig.Claims{ID: "a", Version: 3, SHA256: strings.Repeat("ab", 32), AdmittedBy: "cur", AdmittedAt: time.Now().UTC().Format(time.RFC3339),
		ScanVerdict: "pass", ScannerVersion: "skillscan/2"}
	tok, err := libsig.Sign(priv, c)
	if err != nil || len(tok) > libsig.MaxToken {
		t.Fatalf("sign: %v %d", err, len(tok))
	}
	if _, err := libsig.Verify(pub, "a", "3", c.SHA256, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := libsig.Verify(pub2, "a", "3", c.SHA256, tok); err == nil {
		t.Fatal("another key verified")
	}
	if _, err := libsig.Verify(nil, "a", "3", c.SHA256, tok); err == nil {
		t.Fatal("no key verified")
	}
	if _, err := libsig.Verify(pub, "b", "3", c.SHA256, tok); err == nil {
		t.Fatal("another id verified")
	}
	for _, bad := range []string{"", "!!", tok[:len(tok)-4], tok + "AAAA"} {
		if _, err := libsig.Verify(pub, "a", "3", c.SHA256, bad); err == nil {
			t.Fatalf("token %q verified", bad)
		}
	}
	c.ScanVerdict = "reject"
	tok, _ = libsig.Sign(priv, c)
	if _, err := libsig.Verify(pub, "a", "3", c.SHA256, tok); err == nil {
		t.Fatal("a reject verdict verified")
	}
	if got := string(libsig.Message(libsig.Claims{ID: "a", Version: 1, SHA256: "h", AdmittedBy: "<b>", AdmittedAt: "t", ScanVerdict: "pass", ScannerVersion: "s"})); got !=
		"rubi-skill-library-v2\n"+`{"id":"a","version":1,"sha256":"h","admitted_by":"<b>","admitted_at":"t","scan_verdict":"pass","scanner_version":"s","self_admitted":false,"acked":[]}` {
		t.Fatalf("message: %q", got)
	}
}

// tooBig is a skill the scan rejects for its size.
func tooBig(name string) string {
	return skillNamed(name) + strings.Repeat("a b c d e f g h i j k l m n o p q r s t u v w x y z\n", 640)
}

func TestOpenProposalsPerBotAndAdmittedIDs(t *testing.T) {
	r := newRig(t)
	for i := 0; i < MaxOpenPerBot; i++ {
		id := fmt.Sprintf("s%d", i)
		if _, err := r.l.Submit(Submission{ID: id, Description: "d", Content: skillNamed(id), Author: "Bot-A"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.l.Submit(Submission{ID: "extra", Description: "d", Content: skillNamed("extra"), Author: "bot-a"}); err == nil || !strings.Contains(err.Error(), "waiting") {
		t.Fatalf("a sixth open proposal: %v", err)
	}
	if _, err := r.l.Submit(Submission{ID: "extra", Description: "d", Content: skillNamed("extra"), Author: "bot-b"}); err != nil {
		t.Fatalf("another Bot has its own quota: %v", err)
	}
	// Admitting frees a slot.
	if _, err := r.l.Admit("s0@1", "curator", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.l.Submit(Submission{ID: "extra2", Description: "d", Content: skillNamed("extra2"), Author: "bot-a"}); err != nil {
		t.Fatal(err)
	}
	// A skill with an admitted version takes new versions from the curator only.
	if _, err := r.l.Submit(Submission{ID: "s0", Description: "d", Content: skillNamed("s0") + "evil", Author: "bot-b"}); err == nil || !strings.Contains(err.Error(), "only the curator") {
		t.Fatalf("%v", err)
	}
	if _, _, err := r.l.Publish(Submission{ID: "s0", Description: "d", Content: skillNamed("s0") + "better", Author: "curator"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyLiveVersionsAndEntriesCount(t *testing.T) {
	r := newRig(t)
	r.l.opt.MaxOpenPerBot = 100000
	// Withdrawn and rejected versions don't use up the 20 live slots, and entries made only of them don't use up
	// the entry limit.
	retire := func(id, content string) {
		t.Helper()
		p, err := r.l.Submit(Submission{ID: id, Description: "d", Content: content, Author: "a", Curator: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.l.Admit(p.ProposalID, "curator", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := r.l.SetState(id, p.Version, Withdrawn, "curator"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < MaxVersionsPerItem+3; i++ {
		retire("old", skillNamed("old")+fmt.Sprint(i))
	}
	if p, err := r.l.Submit(Submission{ID: "old", Description: "d", Content: tooBig("old"), Author: "a", Curator: true}); err != nil || p.State != Rejected {
		t.Fatalf("%+v %v", p, err)
	}
	r.l.opt.MaxEntries = 4
	for i := 0; i < 3*r.l.opt.MaxEntries; i++ { // more than the limit, none of them live
		id := fmt.Sprintf("r%d", i)
		retire(id, skillNamed(id))
	}
	if list, _ := r.l.List("", "", true); len(list) != 3*r.l.opt.MaxEntries+1 {
		t.Fatalf("%d", len(list))
	}
	if _, err := r.l.Submit(Submission{ID: "good", Description: "d", Content: skillNamed("good"), Author: "a"}); err != nil {
		t.Fatalf("retired entries used up the live ones: %v", err)
	}
}

func TestPurge(t *testing.T) {
	r := newRig(t)
	bad := tooBig("layout-notes")
	p1 := submit(t, r.l, "bot-a", skill)
	p2 := submit(t, r.l, "bot-a", skill+"two\n")
	if _, err := r.l.Submit(Submission{ID: "layout-notes", Description: "d", Content: bad, Author: "bot-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.l.Admit(p1.ProposalID, "curator", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.l.Purge("layout-notes", 1, "curator"); err == nil {
		t.Fatal("purged an admitted version")
	}
	got, err := r.l.Purge("layout-notes", 0, "curator")
	if err != nil || len(got) != 2 { // the proposal and the reject
		t.Fatalf("%v %v", got, err)
	}
	// A withdrawn version loses its content but keeps its number and its revocation.
	if _, err := r.l.SetState("layout-notes", 1, Withdrawn, "curator"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.l.Purge("layout-notes", 1, "curator"); err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if rev, _ := r.l.Revoked(); len(rev) != 1 || rev[0] != "layout-notes@1" {
		t.Fatalf("%v", rev)
	}
	if _, err := r.l.Admit("layout-notes@1", "curator", nil); err == nil {
		t.Fatal("admitted a purged version")
	}
	_ = p2
	np := submit(t, r.l, "bot-a", skill+"three\n")
	if np.Version <= 3 {
		t.Fatalf("a purged number came back: %d", np.Version)
	}
	// An entry with nothing left is removed, and numbering goes on.
	if _, err := r.l.Purge("layout-notes", np.Version, "curator"); err != nil {
		t.Fatal(err)
	}
}

func TestOlderFileIsRefusedAndOneBadFileDoesNotBreakList(t *testing.T) {
	r := newRig(t)
	a := submit(t, r.l, "bot-a", skill)
	submit(t, r.l, "bot-a", skillNamed("other"))
	old, _ := os.ReadFile(filepath.Join(r.dir, "layout-notes.enc"))
	if _, err := r.l.Admit(a.ProposalID, "curator", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.l.SetState("layout-notes", 1, Withdrawn, "curator"); err != nil {
		t.Fatal(err)
	}
	// Put the older (proposed) file back over the withdrawn one: refused.
	if err := os.WriteFile(filepath.Join(r.dir, "layout-notes.enc"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	r2 := Open(Options{Dir: r.dir, Instance: r.l.opt.Instance, DEK: r.l.opt.DEK, Signer: r.l.opt.Signer})
	if _, err := r2.Get("layout-notes", 1, true); err == nil || !strings.Contains(err.Error(), "older copy") {
		t.Fatalf("%v", err)
	}
	var logged []string
	r2.opt.Logf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	list, err := r2.List("", "", true)
	if err != nil || len(list) != 1 || list[0].ID != "other" || len(logged) != 1 {
		t.Fatalf("%+v %v %v", list, err, logged)
	}
	if _, err := r2.Queue(); err != nil {
		t.Fatal(err)
	}
}
