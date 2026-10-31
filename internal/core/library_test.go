package core

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/library"
	"github.com/Deikus-LXXVII/rubi/internal/library/libsig"
	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

const libSkill = "---\nname: layout-notes\ndescription: How to lay out notes.\n---\n# Layout\nUse short headings.\n"
const libFlagged = "---\nname: fetcher\ndescription: Fetch things.\n---\n# Fetch\nRead https://unlisted.example.org/docs first.\n"

// libCore is targetCore (Main and Mail have webhooks) with Main made the curator through the real approval.
func libCore(t *testing.T) *Core {
	t.Helper()
	c := targetCore(t)
	dek, _ := c.Vault.Key()
	c.Audit.SetVaultKey(dek) // the audit log seals its fields only once Rubi was unlocked or paired
	setCurator(t, c, "Main")
	return c
}

func setCurator(t *testing.T, c *Core, name string) {
	t.Helper()
	res, err := c.RequestLibraryCurator(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	id := res["approval_id"].(string)
	if _, err := c.Approvals.DecidePanel(context.Background(), id, "apply", true, approvals.Verified); err != nil {
		t.Fatal(err)
	}
}

func code(c *Core, bot string) string { return c.agentCodeFor(bot) }

func submitAs(t *testing.T, c *Core, bot, content string) library.Proposal {
	t.Helper()
	p, err := c.LibrarySubmit(bot, code(c, bot), library.Submission{Description: "d", Content: content})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func auditEvents(t *testing.T, c *Core) []map[string]any {
	t.Helper()
	dek, _ := c.Vault.Key()
	k := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, dek, nil, []byte("rubi audit v1")), k); err != nil {
		t.Fatal(err)
	}
	aead, _ := chacha20poly1305.NewX(k)
	raw, _ := os.ReadFile(c.Layout.Audit())
	var out []map[string]any
	for _, ln := range strings.Split(string(raw), "\n") {
		if ln == "" {
			continue
		}
		var l struct{ TS, Event, Sealed string }
		_ = json.Unmarshal([]byte(ln), &l)
		f := map[string]any{"event": l.Event}
		if l.Sealed != "" {
			b, _ := base64.StdEncoding.DecodeString(l.Sealed)
			plain, err := aead.Open(nil, b[:aead.NonceSize()], b[aead.NonceSize():], []byte(l.TS+"|"+l.Event))
			if err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal(plain, &f)
			f["event"] = l.Event
		}
		out = append(out, f)
	}
	return out
}

func auditFor(t *testing.T, c *Core, event string) []map[string]any {
	var out []map[string]any
	for _, e := range auditEvents(t, c) {
		if e["event"] == event {
			out = append(out, e)
		}
	}
	return out
}

func TestLibraryCuratorOnlyThroughStrongApproval(t *testing.T) {
	c := targetCore(t)
	ctx := context.Background()
	if c.LibraryCurator() != "" {
		t.Fatal("a curator before anyone set one")
	}
	if c.PolicyLevel(libraryCuratorKind) != approvals.Strong {
		t.Fatal("rubi.library.curator is not strong")
	}
	res, err := c.RequestLibraryCurator(ctx, "main")
	if err != nil || res["status"] != "awaiting_approval" || res["level"] != approvals.Strong {
		t.Fatalf("%v %v", res, err)
	}
	id := res["approval_id"].(string)
	if s, _ := c.Approvals.Get(id); s.Kind != "rubi.library.curator" || s.Level != approvals.Strong {
		t.Fatalf("approval: %+v", s)
	}
	if c.LibraryCurator() != "" {
		t.Fatal("curator set before the user approved")
	}
	// The Bot can't approve, and chat-level confirmation doesn't exist for a strong approval.
	if _, err := c.Approvals.ConfirmChat(ctx, id, "Approve"); err == nil {
		t.Fatal("confirmed in chat")
	}
	if c.LibraryCurator() != "" {
		t.Fatal("curator set without the panel")
	}
	if _, err := c.Approvals.DecidePanel(ctx, id, "apply", true, approvals.Verified); err != nil {
		t.Fatal(err)
	}
	if c.LibraryCurator() != "Main" {
		t.Fatalf("curator %q (registered name expected)", c.LibraryCurator())
	}
	if len(auditFor(t, c, "library.curator_set")) != 1 {
		t.Fatal("not audited")
	}
	for _, bad := range []string{"Half", "Nobody"} {
		if _, err := c.RequestLibraryCurator(ctx, bad); err == nil {
			t.Fatalf("%s (no webhook) became a candidate", bad)
		}
	}
	if _, err := c.RequestLibraryCurator(ctx, "MAIN"); err == nil {
		t.Fatal("setting the same curator again")
	}
	// Changing and removing go the same way.
	setCurator(t, c, "Mail")
	if c.LibraryCurator() != "Mail" {
		t.Fatal(c.LibraryCurator())
	}
	setCurator(t, c, "")
	if c.LibraryCurator() != "" {
		t.Fatal("not removed")
	}
	// A stale approval (the curator changed while it waited) doesn't apply.
	r1, _ := c.RequestLibraryCurator(ctx, "Main")
	setCurator(t, c, "Mail")
	if s, _ := c.Approvals.DecidePanel(ctx, r1["approval_id"].(string), "apply", true, approvals.Verified); s.State == approvals.Executed || c.LibraryCurator() != "Mail" {
		t.Fatalf("stale approval applied: %+v %q", s, c.LibraryCurator())
	}
}

func TestLibraryNonCuratorCannotAdmit(t *testing.T) {
	c := libCore(t)
	p := submitAs(t, c, "Mail", libSkill)
	if p.State != library.Proposed {
		t.Fatalf("%+v", p)
	}
	// Mail wrote it and has a valid code, but isn't the curator.
	if _, err := c.LibraryAdmit("Mail", code(c, "Mail"), p.ProposalID, nil); err == nil || !strings.Contains(err.Error(), "curator") {
		t.Fatalf("admit by a non-curator: %v", err)
	}
	// The curator's name with another Bot's code doesn't work either.
	if _, err := c.LibraryAdmit("Main", code(c, "Mail"), p.ProposalID, nil); err == nil {
		t.Fatal("Main admitted with Mail's code")
	}
	if _, err := c.LibraryAdmit("Main", "", p.ProposalID, nil); err == nil {
		t.Fatal("admitted without a code")
	}
	if _, err := c.LibraryAdmit("Half", "x", p.ProposalID, nil); err == nil {
		t.Fatal("a Bot without a webhook admitted")
	}
	if _, err := c.LibraryUpdate("Mail", code(c, "Mail"), library.Submission{Description: "d", Content: libSkill}, nil); err == nil {
		t.Fatal("update by a non-curator")
	}
	for _, to := range []library.State{library.Deprecated, library.Withdrawn} {
		if _, err := c.LibrarySetState("Mail", code(c, "Mail"), "layout-notes", 0, to); err == nil {
			t.Fatalf("%s by a non-curator", to)
		}
	}
	if len(auditFor(t, c, "library.admitted")) != 0 {
		t.Fatal("a refused admit left an admit record")
	}
	// Nothing is served.
	if _, err := c.LibraryGet("", "", "layout-notes", 0); err == nil {
		t.Fatal("proposal served")
	}
	if _, err := c.LibraryGet("Mail", code(c, "Mail"), "layout-notes", 1); err == nil {
		t.Fatal("proposal served to its author")
	}
	l, _ := c.LibraryList("Mail", code(c, "Mail"), "", "")
	if got := l["skills"].([]library.Summary); len(got) != 0 || l["curator"] != false {
		t.Fatalf("list for a Bot: %+v", l)
	}
	// With no curator at all, nobody can admit.
	setCurator(t, c, "")
	if _, err := c.LibraryAdmit("Main", code(c, "Main"), p.ProposalID, nil); err == nil {
		t.Fatal("admitted with no curator set")
	}
}

func TestLibraryCuratorAdmitsAndAuditsSelfAdmission(t *testing.T) {
	c := libCore(t)
	p := submitAs(t, c, "Mail", libSkill)
	// The curator sees the proposal (and its text) before admitting.
	if in, err := c.LibraryGet("Main", code(c, "Main"), p.ID, 1); err != nil || in.State != library.Proposed || in.Content != libSkill {
		t.Fatalf("curator view: %+v %v", in, err)
	}
	res, err := c.LibraryAdmit("main", code(c, "Main"), p.ProposalID, nil)
	if err != nil || res.SelfAdmitted || res.State != library.Admitted {
		t.Fatalf("admit: %+v %v", res, err)
	}
	if ev := auditFor(t, c, "library.admitted"); len(ev) != 1 || ev[0]["by"] != "Main" || ev[0]["note"] != nil {
		t.Fatalf("audit: %v", ev)
	}
	// The curator's own proposal: allowed, marked, audited with the words.
	own := submitAs(t, c, "Main", strings.Replace(libSkill, "layout-notes", "own-skill", 2))
	res, err = c.LibraryAdmit("Main", code(c, "Main"), own.ProposalID, nil)
	if err != nil || !res.SelfAdmitted {
		t.Fatalf("self admit: %+v %v", res, err)
	}
	ev := auditFor(t, c, "library.admitted")
	if len(ev) != 2 || ev[1]["note"] != "self-admitted by curator" {
		t.Fatalf("audit: %v", ev)
	}
	in, err := c.LibraryGet("", "", "own-skill", 0)
	if err != nil || !in.SelfAdmitted || in.AdmittedBy != "Main" || in.AuthorBot != "Main" {
		t.Fatalf("get: %+v %v", in, err)
	}
	pub, _ := c.LibraryPublicKey()
	if cl, err := libsig.Verify(pub, in.ID, "1", in.SHA256, in.Signature); err != nil || !cl.SelfAdmitted {
		t.Fatalf("signature: %+v %v", cl, err)
	}
	// Update publishes in one step, as the curator, and is audited.
	up, err := c.LibraryUpdate("Main", code(c, "Main"), library.Submission{ID: "own-skill", Description: "d",
		Content: strings.Replace(libSkill, "layout-notes", "own-skill", 2) + "More.\n"}, nil)
	if err != nil || up.Version != 2 || !up.SelfAdmitted {
		t.Fatalf("update: %+v %v", up, err)
	}
	if ev := auditFor(t, c, "library.published"); len(ev) != 1 || ev[0]["note"] != "self-admitted by curator" {
		t.Fatalf("audit: %v", ev)
	}
	// Deprecate and withdraw are the curator's and are audited.
	if ch, err := c.LibrarySetState("Main", code(c, "Main"), "own-skill", 1, library.Deprecated); err != nil || len(ch) != 1 {
		t.Fatalf("%v %v", ch, err)
	}
	if _, err := c.LibrarySetState("Main", code(c, "Main"), "own-skill", 0, library.Withdrawn); err != nil {
		t.Fatal(err)
	}
	if len(auditFor(t, c, "library.deprecated")) != 1 || len(auditFor(t, c, "library.withdrawn")) != 1 {
		t.Fatal("not audited")
	}
	if _, err := c.LibraryGet("", "", "own-skill", 0); err == nil {
		t.Fatal("withdrawn skill served")
	}
	if len(auditFor(t, c, "library.refused")) != 0 {
		t.Fatal("unexpected refusals")
	}
}

func TestLibraryFlagsNeedAcknowledgementAndRejectsAreFinal(t *testing.T) {
	c := libCore(t)
	p := submitAs(t, c, "Mail", libFlagged)
	if p.Scan.Verdict != "flag" {
		t.Fatalf("%+v", p.Scan)
	}
	if _, err := c.LibraryAdmit("Main", code(c, "Main"), p.ProposalID, nil); err == nil || !strings.Contains(err.Error(), "exf.url") {
		t.Fatalf("admitted without ack: %v", err)
	}
	if _, err := c.LibraryAdmit("Main", code(c, "Main"), p.ProposalID, make([]string, 40)); err == nil {
		t.Fatal("huge ack list taken")
	}
	if res, err := c.LibraryAdmit("Main", code(c, "Main"), p.ProposalID, []string{"exf.url"}); err != nil || len(res.Acked) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if ev := auditFor(t, c, "library.admitted"); len(ev) != 1 || ev[0]["acked"] == nil {
		t.Fatalf("the acknowledged rules are in the audit: %v", ev)
	}
	rej := submitAs(t, c, "Mail", strings.Replace(libSkill, "Use short", "<!-- x -->Use short", 1))
	if rej.State != library.Rejected {
		t.Fatalf("%+v", rej)
	}
	if _, err := c.LibraryAdmit("Main", code(c, "Main"), rej.ProposalID, []string{"hid.comment"}); err == nil {
		t.Fatal("a reject was admitted")
	}
	// Even the curator publishing its own can't pass a reject.
	if _, err := c.LibraryUpdate("Main", code(c, "Main"), library.Submission{ID: "bad-one", Description: "d",
		Content: strings.Replace(strings.Replace(libSkill, "layout-notes", "bad-one", 1), "Use short", "<!-- x -->Use short", 1)}, nil); err == nil {
		t.Fatal("the curator published a reject")
	}
}

func TestLibrarySubmitNeedsAProvenBot(t *testing.T) {
	c := libCore(t)
	in := library.Submission{Description: "d", Content: libSkill}
	for name, args := range map[string][2]string{"no name": {"", ""}, "no code": {"Mail", ""}, "wrong code": {"Mail", "nope"},
		"others code": {"Mail", code(c, "Main")}, "no webhook": {"Half", "x"}} {
		if _, err := c.LibrarySubmit(args[0], args[1], in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	p, err := c.LibrarySubmit("mail", code(c, "Mail"), in)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Library.Get(p.ID, 1, true); v.AuthorBot != "Mail" {
		t.Fatalf("author %q (registered name expected)", v.AuthorBot)
	}
	c.Lock()
	if _, err := c.LibrarySubmit("Mail", "x", in); err == nil {
		t.Fatal("submit while locked")
	}
	if _, err := c.LibraryList("", "", "", ""); err == nil {
		t.Fatal("list while locked")
	}
}

func TestLibrarySubmitIsRateLimited(t *testing.T) {
	c := libCore(t)
	var failed error
	for i := 0; i < librarySubmitsPerMin+2 && failed == nil; i++ {
		_, failed = c.LibrarySubmit("Main", code(c, "Main"), library.Submission{ID: "a" + string(rune('a'+i)), Description: "d",
			Content: strings.Replace(libSkill, "layout-notes", "a"+string(rune('a'+i)), 1)})
	}
	if failed == nil || !strings.Contains(failed.Error(), "slow down") {
		t.Fatalf("no limit: %v", failed)
	}
}

func TestLibraryKeyLivesInTheVaultAndIsStable(t *testing.T) {
	c := libCore(t)
	k1, err := c.LibraryPublicKey()
	if err != nil || len(k1) != ed25519.PublicKeySize {
		t.Fatalf("%v", err)
	}
	k2, _ := c.LibraryPublicKey()
	if !k1.Equal(k2) {
		t.Fatal("the key changed between uses")
	}
	var seed []byte
	_ = c.Vault.View(func(d *vault.Data) error { seed = d.LibraryKey; return nil })
	if len(seed) != ed25519.SeedSize || !ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey).Equal(k1) {
		t.Fatal("the key isn't the vault's")
	}
	// Not in the clear anywhere on disk.
	filepathWalkNoSeed(t, c.Layout.Home, seed)
	c.Lock()
	if _, err := c.LibraryPublicKey(); err == nil {
		t.Fatal("key used while locked")
	}
}

func filepathWalkNoSeed(t *testing.T, dir string, seed []byte) {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		p := dir + "/" + e.Name()
		if e.IsDir() {
			filepathWalkNoSeed(t, p, seed)
			continue
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), base64.StdEncoding.EncodeToString(seed)) || strings.Contains(string(b), string(seed)) {
			t.Fatalf("library key seed in the clear in %s", p)
		}
	}
}

// ---- the plugin host method and the pinning op ----

const claudeAgentPlugin = `{"schema":1,"id":"agent-plugin","name":"Agent","version":"v1.0.0","api":1,
	"entry":"agent-plugin","publisher":{"name":"x","key":"MCowBQYDK2VwAyEAxeDfKAkO77JdARN7Y2jJT3tXw9mN+GqqH8R5mhcxt8c="},
	"home":["claude-agent"]}`

func TestLibraryHostMethod(t *testing.T) {
	c := libCore(t)
	installManifest(t, c, claudeAgentPlugin)
	installManifest(t, c, strings.NewReplacer("agent-plugin", "other-plugin", `["claude-agent"]`, `["hue"]`).Replace(claudeAgentPlugin))
	_ = c.Vault.Update(func(d *vault.Data) error {
		d.Integrations["agent-plugin"] = &vault.Integration{Enabled: true}
		d.Integrations["other-plugin"] = &vault.Integration{Enabled: true}
		return nil
	})
	call := func(plugin string, params string) (map[string]any, error) {
		res, err := c.pluginHandler(plugin)(context.Background(), "library.get", json.RawMessage(params))
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(res)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m, nil
	}
	p := submitAs(t, c, "Mail", libSkill)
	for _, params := range []string{`{"id":"layout-notes"}`, `{"id":"layout-notes","version":1}`} {
		if _, err := call("agent-plugin", params); err == nil {
			t.Fatalf("a proposal was served to a plugin: %s", params)
		}
	}
	if _, err := c.LibraryAdmit("Main", code(c, "Main"), p.ProposalID, nil); err != nil {
		t.Fatal(err)
	}
	got, err := call("agent-plugin", `{"id":"layout-notes","version":1}`)
	if err != nil || got["content"] != libSkill || got["signature"] == "" || got["state"] != "admitted" || got["admitted_by"] != "Main" {
		t.Fatalf("%v %v", got, err)
	}
	pub, _ := c.LibraryPublicKey()
	if _, err := libsig.Verify(pub, "layout-notes", "1", got["sha256"].(string), got["signature"].(string)); err != nil {
		t.Fatal(err)
	}
	if latest, err := call("agent-plugin", `{"id":"layout-notes"}`); err != nil || latest["version"] != float64(1) {
		t.Fatalf("%v %v", latest, err)
	}
	if _, err := call("other-plugin", `{"id":"layout-notes"}`); err == nil || !strings.Contains(err.Error(), "claude-agent") {
		t.Fatalf("a plugin without the capability: %v", err)
	}
	if _, err := call("agent-plugin", `{"id":"nope"}`); err == nil {
		t.Fatal("missing skill")
	}
	if _, err := call("agent-plugin", `{"id":"../x"}`); err == nil {
		t.Fatal("odd id")
	}
	// Withdrawn: gone for plugins too.
	if _, err := c.LibrarySetState("Main", code(c, "Main"), "layout-notes", 0, library.Withdrawn); err != nil {
		t.Fatal(err)
	}
	if _, err := call("agent-plugin", `{"id":"layout-notes","version":1}`); err == nil {
		t.Fatal("withdrawn skill served to a plugin")
	}
	// There is no host method that writes.
	for _, m := range []string{"library.put", "library.admit", "library.submit", "library.key"} {
		if _, err := c.pluginHandler("agent-plugin")(context.Background(), m, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("host method %s exists", m)
		}
	}
}

// The op that pins the library key on Rubi Home is core-only: Home can't tell a core request from a plugin's
// home.call relayed by core, so the filter in homeCall is what keeps plugins away from it.
func TestPluginCannotCallTheLibraryKeyOp(t *testing.T) {
	c := libCore(t)
	all := []string{"hue", "shortcuts", "claude", "claude-sessions", "claude-start", "claude-agent", "agent-setup"}
	for _, op := range []string{"library.key", "library.get", "library", "library.", "Library.key", " library.key", "claude-agentlibrary.key", "library.key/../claude.next"} {
		_, err := c.homeCall(context.Background(), "p", all, "", op, json.RawMessage(`{"public_key":"x"}`))
		if err == nil || !strings.Contains(err.Error(), "doesn't allow") {
			t.Errorf("op %q: %v", op, err)
		}
	}
	// A manifest can't declare a "library" capability to get past the prefix check.
	var m plugins.Manifest
	if err := json.Unmarshal([]byte(strings.Replace(claudeAgentPlugin, `["claude-agent"]`, `["library"]`, 1)), &m); err != nil {
		t.Fatal(err)
	}
	if err := plugins.Check(&m); err == nil {
		t.Fatal("a manifest with the library capability was accepted")
	}
	if _, ok := homeCapabilities["library"]; ok {
		t.Fatal("library is a plugin-visible Rubi Home capability")
	}
	// Through the host method a plugin with every capability is refused too.
	installManifest(t, c, strings.Replace(claudeAgentPlugin, `["claude-agent"]`, `["hue","shortcuts","claude","claude-sessions","claude-start","claude-agent","agent-setup"]`, 1))
	_ = c.Vault.Update(func(d *vault.Data) error {
		d.Integrations["agent-plugin"] = &vault.Integration{Enabled: true}
		return nil
	})
	_, err := c.pluginHandler("agent-plugin")(context.Background(), "home.call", json.RawMessage(`{"op":"library.key","args":{"public_key":"x"}}`))
	if err == nil || !strings.Contains(err.Error(), "doesn't allow") {
		t.Fatalf("home.call library.key: %v", err)
	}
}

func TestLibraryPurgeAndAdmittedIDsAreTheCuratorsOnly(t *testing.T) {
	c := libCore(t)
	p := submitAs(t, c, "Main", libSkill)
	if _, err := c.LibraryAdmit("Main", code(c, "Main"), p.ProposalID, nil); err != nil {
		t.Fatal(err)
	}
	// Another Bot can't add a version to an admitted skill; the curator's own submit still can.
	if _, err := c.LibrarySubmit("Mail", code(c, "Mail"), library.Submission{Description: "d", Content: libSkill + "x\n"}); err == nil ||
		!strings.Contains(err.Error(), "only the curator") {
		t.Fatalf("%v", err)
	}
	if _, err := c.LibrarySubmit("Main", code(c, "Main"), library.Submission{Description: "d", Content: libSkill + "y\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LibraryPurge("Mail", code(c, "Mail"), "layout-notes", 0); err == nil {
		t.Fatal("a non-curator purged")
	}
	got, err := c.LibraryPurge("Main", code(c, "Main"), "layout-notes", 0)
	if err != nil || len(got) != 1 || got[0] != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if len(auditFor(t, c, "library.purged")) != 1 {
		t.Fatal("not audited")
	}
}

type homeCalls struct {
	mu    sync.Mutex
	calls []string
	args  []map[string]any
	fail  atomic.Bool
	slow  chan struct{}
}

func (h *homeCalls) call(ctx context.Context, dev *vault.Device, op string, args any) error {
	if h.slow != nil {
		select {
		case <-h.slow:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if h.fail.Load() {
		return errors.New("offline")
	}
	b, _ := json.Marshal(args)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	h.mu.Lock()
	h.calls = append(h.calls, dev.ID+" "+op)
	h.args = append(h.args, m)
	h.mu.Unlock()
	return nil
}

func (h *homeCalls) count(op string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if strings.HasSuffix(c, " "+op) {
			n++
		}
	}
	return n
}

func withDevice(t *testing.T, c *Core, id string) {
	t.Helper()
	if err := c.Vault.Update(func(d *vault.Data) error {
		d.Devices = append(d.Devices, &vault.Device{ID: id, Name: id})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out: " + what)
}

// A withdrawal reaches every Home as a signed list; a Home that is down doesn't make library.get wait.
func TestLibraryWithdrawalIsPushedSignedAndAnOfflineHomeDoesNotStallGet(t *testing.T) {
	c := libCore(t)
	h := &homeCalls{}
	c.libPins.call = h.call
	withDevice(t, c, "dev-1")
	withDevice(t, c, "dev-2")
	p := submitAs(t, c, "Mail", libSkill)
	if _, err := c.LibraryAdmit("Main", code(c, "Main"), p.ProposalID, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the push after admitting", func() bool { return h.count("library.revoked") == 2 })
	if _, err := c.LibrarySetState("Main", code(c, "Main"), "layout-notes", 1, library.Withdrawn); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the push after withdrawing", func() bool { return h.count("library.revoked") == 4 })
	h.mu.Lock()
	last := h.args[len(h.args)-1]
	h.mu.Unlock()
	pub, _ := c.LibraryPublicKey()
	list := []string{}
	for _, x := range last["revoked"].([]any) {
		list = append(list, x.(string))
	}
	if len(list) != 1 || list[0] != "layout-notes@1" {
		t.Fatalf("%v", last)
	}
	if err := libsig.VerifyRevocations(pub, int64(last["issued_at"].(float64)), list, last["sig"].(string)); err != nil {
		t.Fatal(err)
	}

	// Offline Homes: library.get returns at once, and a Home that just failed isn't tried again for a while.
	c2 := libCore(t)
	off := &homeCalls{slow: make(chan struct{})}
	c2.libPins.call = off.call
	withDevice(t, c2, "dev-1")
	withDevice(t, c2, "dev-2")
	start := time.Now()
	c2.pinLibraryKeys(context.Background())
	if d := time.Since(start); d > libraryPinWait+time.Second {
		t.Fatalf("library.get waited %v for offline Homes", d)
	}
	close(off.slow)
	off.fail.Store(true)
	waitFor(t, "failures noted", func() bool {
		c2.libPins.mu.Lock()
		defer c2.libPins.mu.Unlock()
		return len(c2.libPins.failed) == 2
	})
	off.fail.Store(false)
	start = time.Now()
	c2.pinLibraryKeys(context.Background())
	if time.Since(start) > 200*time.Millisecond || off.count("library.key") != 0 {
		t.Fatal("an offline Home was tried again at once")
	}
}
