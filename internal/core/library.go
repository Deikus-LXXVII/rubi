package core

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/audit"
	"github.com/Deikus-LXXVII/rubi/internal/library"
	"github.com/Deikus-LXXVII/rubi/internal/library/libsig"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// The shared skill library (docs/design/agent-roles.md section 8). Bots submit; one curator Bot admits; admitting
// signs the version with the library key, which lives in the vault (so it is usable only while Rubi is unlocked)
// and whose public half Rubi Home pins.
//
// Pinning. Rubi Home cannot tell a core-originated op from a plugin's home.call relayed by core: both arrive
// with the same pairing token in the same envelope. What keeps a plugin from sending the pinning op is core's
// homeCall filter: an op must start with "<capability>." for a capability the plugin's manifest declares, and the
// manifest validator accepts only the capabilities in homeCapabilities, none of which is "library". So the op
// "library.key" is reachable only through pinLibraryKey below, never from a plugin.

const (
	libraryCuratorKind    = "rubi.library.curator"
	libraryWithdrawKind   = "rubi.library.withdraw"
	librarySubmitsPerMin  = 10
	libraryKeyPinOp       = "library.key"
	libraryRevokedOp      = "library.revoked"
	libraryPinTimeout     = 8 * time.Second
	libraryPinEvery       = 10 * time.Minute // Home may have lost its pin (reinstalled); pinning again is harmless
	maxLibraryAckRules    = 32
	selfAdmittedAuditNote = "self-admitted by curator"
)

type pinned struct {
	key string
	at  time.Time
}

// libraryKey returns the library signing key, making it on first use. It fails while Rubi is locked.
func (c *Core) libraryKey() (ed25519.PrivateKey, error) {
	var key ed25519.PrivateKey
	err := c.Vault.View(func(d *vault.Data) error {
		if len(d.LibraryKey) == ed25519.SeedSize {
			key = ed25519.NewKeyFromSeed(d.LibraryKey)
		}
		return nil
	})
	if err != nil || key != nil {
		return key, err
	}
	err = c.Vault.Update(func(d *vault.Data) error {
		if len(d.LibraryKey) != ed25519.SeedSize {
			seed := make([]byte, ed25519.SeedSize)
			if _, err := rand.Read(seed); err != nil {
				return err
			}
			d.LibraryKey = seed
		}
		key = ed25519.NewKeyFromSeed(d.LibraryKey)
		return nil
	})
	return key, err
}

// LibraryPublicKey is the public half of the library key (made on first use).
func (c *Core) LibraryPublicKey() (ed25519.PublicKey, error) {
	k, err := c.libraryKey()
	if err != nil {
		return nil, err
	}
	return k.Public().(ed25519.PublicKey), nil
}

// LibraryCurator is the Bot that curates the library ("" = none).
func (c *Core) LibraryCurator() string {
	var name string
	_ = c.Vault.View(func(d *vault.Data) error { name = d.LibraryCurator; return nil })
	return name
}

// RequestLibraryCurator asks the user, with a strong approval (kind rubi.library.curator), to make the Bot called
// name the library's curator; "" removes the curator. Nothing changes until the user approves in the panel.
func (c *Core) RequestLibraryCurator(ctx context.Context, name string) (map[string]any, error) {
	if c.State() != Unlocked {
		return nil, errors.New("Rubi is locked")
	}
	name = strings.TrimSpace(name)
	var cur string
	if name != "" {
		reg, ok := c.webhookAgent(name)
		if !ok {
			return nil, fmt.Errorf("no Bot named %q is connected to Rubi with a webhook (agents: %s)", oneLineName(name), c.agentNames())
		}
		name = reg
	}
	cur = c.LibraryCurator()
	if strings.EqualFold(cur, name) {
		return nil, errors.New("that is already the curator")
	}
	summary := "Make " + name + " the skill library's curator"
	if name == "" {
		summary = "Remove the skill library's curator"
	}
	show := func(s string) string {
		if s == "" {
			return "(none)"
		}
		return s
	}
	return c.Approvals.Submit(ctx, approvals.Request{
		Integration: "rubi", Kind: libraryCuratorKind, Summary: summary,
		Preview: map[string]any{"curator now": show(cur), "curator after": show(name),
			"allows": "the curator Bot can admit skills into the shared library, publish new versions, deprecate and withdraw them. Admitted skills are trusted by every role that uses them, and the curator may admit its own. The automatic scan still has to pass."},
		Options: []approvals.Option{{Key: "apply", Label: "Approve"}},
		Execute: func(context.Context, string) (any, error) {
			if err := c.Vault.Update(func(d *vault.Data) error {
				if !strings.EqualFold(d.LibraryCurator, cur) {
					return errors.New("the curator changed while this waited; ask again")
				}
				d.LibraryCurator = name
				return nil
			}); err != nil {
				return nil, err
			}
			c.Audit.Record("library.curator_set", audit.Fields{"from": cur, "to": name})
			return map[string]any{"applied": true, "curator": name}, nil
		},
	})
}

// RequestLibraryWithdraw asks the user, with a strong approval (kind rubi.library.withdraw), to withdraw a
// version of a library skill (version 0: every version that isn't withdrawn). This is the user's own way to pull a
// skill the curator should not have admitted, whatever the curator does. Nothing changes until the user approves.
func (c *Core) RequestLibraryWithdraw(ctx context.Context, id string, version int) (map[string]any, error) {
	if c.State() != Unlocked {
		return nil, errors.New("Rubi is locked")
	}
	if version < 0 {
		return nil, errors.New("bad version")
	}
	vs, err := c.Library.Entry(id)
	if err != nil {
		return nil, err
	}
	var which []string
	for _, v := range vs {
		if (version == 0 || v.Version == version) && v.State != library.Withdrawn && v.State != library.Rejected {
			which = append(which, "v"+strconv.Itoa(v.Version)+" ("+string(v.State)+")")
		}
	}
	if len(which) == 0 {
		return nil, errors.New("there is nothing to withdraw there")
	}
	summary := "Withdraw skill " + id + " from the library"
	if version != 0 {
		summary = "Withdraw version " + strconv.Itoa(version) + " of skill " + id + " from the library"
	}
	return c.Approvals.Submit(ctx, approvals.Request{
		Integration: "rubi", Kind: libraryWithdrawKind, Summary: summary,
		Preview: map[string]any{"skill": id, "versions": strings.Join(which, ", "),
			"effect": "Roles that use it drop it the next time Rubi Home builds them (at once on computers that are online, otherwise when they next connect to Rubi). New roles can't attach it. The curator can admit it again."},
		Options: []approvals.Option{{Key: "apply", Label: "Withdraw"}},
		Execute: func(context.Context, string) (any, error) {
			changed, err := c.Library.SetState(id, version, library.Withdrawn, "the user (panel)")
			if err != nil {
				return nil, err
			}
			c.Audit.Record("library.withdrawn", audit.Fields{"id": id, "versions": changed, "by": "the user"})
			c.pushLibraryRevocations()
			return map[string]any{"applied": true, "withdrawn": changed}, nil
		},
	})
}

// libraryBot proves who is calling (the Bot's registered name).
func (c *Core) libraryBot(name, code string) (string, error) {
	if c.State() != Unlocked {
		return "", errors.New("Rubi is locked")
	}
	reg, ok := c.webhookAgent(name)
	if !ok {
		return "", fmt.Errorf("agent: your Bot's name as connected to Rubi with a webhook (agents: %s)", c.agentNames())
	}
	if err := c.checkAgentCode(reg, code); err != nil {
		if errors.Is(err, ErrAgentCode) {
			return "", errors.New("code: pass the agent_code of your latest Rubi webhook (or call rubi_verify with your Bot name to get one)")
		}
		return "", err
	}
	return reg, nil
}

// libraryCuratorBot is libraryBot, and the Bot must be the curator.
func (c *Core) libraryCuratorBot(name, code string) (string, error) {
	reg, err := c.libraryBot(name, code)
	if err != nil {
		return "", err
	}
	cur := c.LibraryCurator()
	if cur == "" || !strings.EqualFold(cur, reg) {
		c.Audit.Record("library.refused", audit.Fields{"agent": reg, "why": "not the curator"})
		return "", errors.New("only the library's curator can do this; the user picks the curator in Rubi's settings")
	}
	return reg, nil
}

// libraryReader says whether the caller (optionally proven) is the curator and may see every state.
func (c *Core) libraryReader(name, code string) (bool, error) {
	if strings.TrimSpace(name) == "" {
		return false, nil
	}
	reg, err := c.libraryBot(name, code)
	if err != nil {
		return false, err
	}
	cur := c.LibraryCurator()
	return cur != "" && strings.EqualFold(cur, reg), nil
}

func (c *Core) libraryUnlocked() error {
	if c.State() != Unlocked {
		return errors.New("Rubi is locked")
	}
	return nil
}

// LibraryList lists skills. The curator (when it proves itself) also sees proposals and withdrawn versions.
func (c *Core) LibraryList(agent, code, query, tag string) (map[string]any, error) {
	if err := c.libraryUnlocked(); err != nil {
		return nil, err
	}
	cur, err := c.libraryReader(agent, code)
	if err != nil {
		return nil, err
	}
	list, err := c.Library.List(query, tag, cur)
	return map[string]any{"skills": list, "curator": cur}, err
}

// LibraryGet returns one version with its content.
func (c *Core) LibraryGet(agent, code, id string, version int) (library.Info, error) {
	if err := c.libraryUnlocked(); err != nil {
		return library.Info{}, err
	}
	cur, err := c.libraryReader(agent, code)
	if err != nil {
		return library.Info{}, err
	}
	return c.Library.Get(id, version, cur)
}

// LibrarySubmit puts a proposal in the queue.
func (c *Core) LibrarySubmit(agent, code string, in library.Submission) (library.Proposal, error) {
	bot, err := c.libraryBot(agent, code)
	if err != nil {
		return library.Proposal{}, err
	}
	if !c.allow("library.submit:"+agentKey(bot), librarySubmitsPerMin) {
		return library.Proposal{}, errors.New("too many submissions; slow down")
	}
	in.Author = bot
	cur := c.LibraryCurator()
	in.Curator = cur != "" && strings.EqualFold(cur, bot)
	p, err := c.Library.Submit(in)
	if err != nil {
		return p, err
	}
	c.Audit.Record("library.submitted", audit.Fields{"proposal": p.ProposalID, "by": bot, "state": p.State, "verdict": p.Scan.Verdict})
	return p, nil
}

func cleanAck(ack []string) ([]string, error) {
	if len(ack) > maxLibraryAckRules {
		return nil, errors.New("too many rule ids in ack")
	}
	return ack, nil
}

func (c *Core) auditAdmit(ev string, res library.AdmitResult, by string) {
	f := audit.Fields{"proposal": res.ProposalID, "by": by, "acked": res.Acked}
	if res.SelfAdmitted {
		f["note"] = selfAdmittedAuditNote
	}
	c.Audit.Record(ev, f)
}

// LibraryAdmit admits a proposal; only the curator may.
func (c *Core) LibraryAdmit(agent, code, proposalID string, ack []string) (library.AdmitResult, error) {
	by, err := c.libraryCuratorBot(agent, code)
	if err != nil {
		return library.AdmitResult{}, err
	}
	if ack, err = cleanAck(ack); err != nil {
		return library.AdmitResult{}, err
	}
	res, err := c.Library.Admit(proposalID, by, ack)
	if err != nil {
		return res, err
	}
	c.auditAdmit("library.admitted", res, by)
	c.pushLibraryRevocations() // admitting a withdrawn version again takes it off the list
	return res, nil
}

// LibraryUpdate is the curator's new version of a skill, admitted at once (self-admitted by definition).
func (c *Core) LibraryUpdate(agent, code string, in library.Submission, ack []string) (library.AdmitResult, error) {
	by, err := c.libraryCuratorBot(agent, code)
	if err != nil {
		return library.AdmitResult{}, err
	}
	if ack, err = cleanAck(ack); err != nil {
		return library.AdmitResult{}, err
	}
	if !c.allow("library.submit:"+agentKey(by), librarySubmitsPerMin) {
		return library.AdmitResult{}, errors.New("too many submissions; slow down")
	}
	in.Author = by
	res, p, err := c.Library.Publish(in, ack)
	if p.ProposalID != "" && err != nil {
		// The version exists as a proposal (or rejected) even though it wasn't admitted.
		c.Audit.Record("library.submitted", audit.Fields{"proposal": p.ProposalID, "by": by, "state": p.State, "verdict": p.Scan.Verdict})
	}
	if err != nil {
		return res, err
	}
	c.auditAdmit("library.published", res, by)
	return res, nil
}

// LibrarySetState deprecates or withdraws (version 0: all that apply); only the curator may.
func (c *Core) LibrarySetState(agent, code, id string, version int, to library.State) ([]int, error) {
	by, err := c.libraryCuratorBot(agent, code)
	if err != nil {
		return nil, err
	}
	changed, err := c.Library.SetState(id, version, to, by)
	if err != nil {
		return nil, err
	}
	c.Audit.Record("library."+string(to), audit.Fields{"id": id, "versions": changed, "by": by})
	if to == library.Withdrawn {
		c.pushLibraryRevocations()
	}
	return changed, nil
}

// LibraryPurge drops rejected, unadmitted and withdrawn versions (version 0: all of a skill's); only the curator may.
func (c *Core) LibraryPurge(agent, code, id string, version int) ([]int, error) {
	by, err := c.libraryCuratorBot(agent, code)
	if err != nil {
		return nil, err
	}
	purged, err := c.Library.Purge(id, version, by)
	if err != nil {
		return nil, err
	}
	c.Audit.Record("library.purged", audit.Fields{"id": id, "versions": purged, "by": by})
	return purged, nil
}

// ---- telling Rubi Home about the library ----

// Two core-only ops go to every paired Rubi Home: library.key (the public key that signs the library; Home pins
// the first one it is given and refuses another) and library.revoked (the signed list of withdrawn versions; Home
// keeps the newest, refuses to build roles with those versions and takes them out of the roles it has built).
// Only core calls them; see the note at the top of this file.
//
// Latency of a withdrawal: a Home that is online gets the list within seconds of the withdrawal (it is pushed from
// a goroutine, and again whenever a plugin fetches a skill). A Home that is offline, or whose relay is down, gets it
// the next time a plugin fetches a skill or a withdrawal happens, or when Rubi restarts and a role is fetched, and
// until then it keeps using what it had built. A VM start on Home also re-checks against the list Home holds.

const (
	libraryPinFailCooldown = time.Minute // an offline Home isn't tried again for this long by a plugin's library.get
	libraryPinWait         = 2 * time.Second
)

type libraryPins struct {
	mu       sync.Mutex
	pinned   map[string]pinned    // device id -> what this process last pinned there
	failed   map[string]time.Time // device id -> when the last attempt failed
	inflight map[string]bool
	revSent  map[string]revSent // device id -> the list last pushed there
	// call is the seam for tests: it sends one op to a Home (nil: through the device's client).
	call func(ctx context.Context, dev *vault.Device, op string, args any) error
}

type revSent struct {
	hash string
	at   time.Time
}

func (c *Core) homeCallLibrary(ctx context.Context, dev *vault.Device, op string, args any) error {
	if c.libPins.call != nil {
		return c.libPins.call(ctx, dev, op, args)
	}
	cl, err := c.deviceClient(dev)
	if err != nil {
		return err
	}
	return cl.Call(ctx, op, args, nil)
}

// syncLibraryHome pins the key on one Rubi Home and gives it the list of withdrawn versions (cached for a while
// unless force is set, or the list changed).
func (c *Core) syncLibraryHome(ctx context.Context, dev *vault.Device, force bool) error {
	pub, err := c.LibraryPublicKey()
	if err != nil {
		return err
	}
	enc := base64.StdEncoding.EncodeToString(pub)
	revoked, err := c.Library.Revoked()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(strings.Join(revoked, "\n")))
	hash := hex.EncodeToString(sum[:])
	c.libPins.mu.Lock()
	last := c.libPins.pinned[dev.ID]
	sent := c.libPins.revSent[dev.ID]
	pinDone := last.key == enc && time.Since(last.at) < libraryPinEvery && !force
	revDone := sent.hash == hash && time.Since(sent.at) < libraryPinEvery && !force
	c.libPins.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, libraryPinTimeout)
	defer cancel()
	if !pinDone {
		if err := c.homeCallLibrary(ctx, dev, libraryKeyPinOp, map[string]string{"public_key": enc}); err != nil {
			return err
		}
		c.libPins.mu.Lock()
		if c.libPins.pinned == nil {
			c.libPins.pinned = map[string]pinned{}
		}
		c.libPins.pinned[dev.ID] = pinned{enc, time.Now()}
		c.libPins.mu.Unlock()
	}
	if revDone {
		return nil
	}
	priv, err := c.libraryKey()
	if err != nil {
		return err
	}
	at := time.Now().UnixMilli()
	sig, err := libsig.SignRevocations(priv, at, revoked)
	if err != nil {
		return err
	}
	if err := c.homeCallLibrary(ctx, dev, libraryRevokedOp, map[string]any{"issued_at": at, "revoked": revoked, "sig": sig}); err != nil {
		return err
	}
	c.libPins.mu.Lock()
	if c.libPins.revSent == nil {
		c.libPins.revSent = map[string]revSent{}
	}
	c.libPins.revSent[dev.ID] = revSent{hash, time.Now()}
	c.libPins.mu.Unlock()
	return nil
}

func (c *Core) libraryDevices() []*vault.Device {
	var devs []*vault.Device
	_ = c.Vault.View(func(d *vault.Data) error {
		for _, x := range d.Devices {
			cp := *x
			devs = append(devs, &cp)
		}
		return nil
	})
	return devs
}

// startLibrarySync starts syncing every paired Home that isn't already being synced (and, unless force is set,
// didn't just fail) and returns channels that close when each is done. Nothing here waits for an offline Home.
func (c *Core) startLibrarySync(force bool) []<-chan struct{} {
	var done []<-chan struct{}
	for _, d := range c.libraryDevices() {
		c.libPins.mu.Lock()
		if c.libPins.inflight == nil {
			c.libPins.inflight, c.libPins.failed = map[string]bool{}, map[string]time.Time{}
		}
		skip := c.libPins.inflight[d.ID] || (!force && time.Since(c.libPins.failed[d.ID]) < libraryPinFailCooldown)
		if !skip {
			c.libPins.inflight[d.ID] = true
		}
		c.libPins.mu.Unlock()
		if skip {
			continue
		}
		ch := make(chan struct{})
		done = append(done, ch)
		go func() {
			defer close(ch)
			err := c.syncLibraryHome(context.Background(), d, force)
			c.libPins.mu.Lock()
			delete(c.libPins.inflight, d.ID)
			if err != nil {
				c.libPins.failed[d.ID] = time.Now()
			} else {
				delete(c.libPins.failed, d.ID)
			}
			c.libPins.mu.Unlock()
		}()
	}
	return done
}

// pinLibraryKeys syncs every paired Rubi Home, waiting at most libraryPinWait (a plugin that fetches a skill goes
// on to role.put at once, so an online Home should have the key by then; an offline one costs no more than that, and
// is not tried again for a minute).
func (c *Core) pinLibraryKeys(ctx context.Context) {
	deadline := time.After(libraryPinWait)
	for _, ch := range c.startLibrarySync(false) {
		select {
		case <-ch:
		case <-deadline:
			return
		case <-ctx.Done():
			return
		}
	}
}

// pushLibraryRevocations sends the list of withdrawn versions to every Home now, without waiting for any.
func (c *Core) pushLibraryRevocations() { c.startLibrarySync(true) }

// ---- host method library.get ----

// libraryHostGet serves a plugin's library.get {id, version}: an admitted (or deprecated) version with its
// signature, so the plugin can build role.put. Read-only. Only a plugin that declares the claude-agent Rubi Home
// capability (the one that has roles) may ask. Asking also makes sure Rubi Home has the key to verify with.
func (c *Core) libraryHostGet(ctx context.Context, home []string, params json.RawMessage) (any, error) {
	ok := false
	for _, h := range home {
		ok = ok || h == "claude-agent"
	}
	if !ok {
		return nil, errors.New("the manifest doesn't declare the claude-agent Rubi Home capability")
	}
	var in struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(params, &in); err != nil {
		return nil, err
	}
	if err := c.libraryUnlocked(); err != nil {
		return nil, err
	}
	info, err := c.Library.GetAdmitted(in.ID, in.Version)
	if err != nil {
		return nil, err
	}
	c.pinLibraryKeys(ctx)
	return info, nil
}
