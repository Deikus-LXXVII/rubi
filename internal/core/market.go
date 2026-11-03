package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/audit"
	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/update"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/internal/version"
	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

const catalogTTL = 10 * time.Minute

type marketState struct {
	mu           sync.Mutex
	catalog      *plugins.Catalog
	fetchedAt    time.Time
	toolsChanged []func()
	staged       map[string]bool // staging dirs of pending install approvals

	// updMu makes plugin update requests one at a time; pendingUpdate is the one plugin-update approval
	// that may wait at a time, so the user gets one link for all updates, however the Bot asks.
	updMu         sync.Mutex
	pendingUpdate *pendingUpdate
	pendingAll    *pendingAll // the "update everything" approval that waits (updateall.go); also guarded by updMu

	// dlMu makes a download, from the cleaning of the staging folder to the moment the result is registered as
	// waiting, one at a time: otherwise another request's cleaning deletes a download that is still under way.
	dlMu sync.Mutex
	// applyMu makes the check and the commit of an install or an update one step.
	applyMu sync.Mutex
}

type pendingUpdate struct {
	approvalID string
	ids        map[string]bool
	res        map[string]any
}

// Catalog returns the signed plugin catalog, cached for a few minutes.
func (c *Core) Catalog(ctx context.Context) (*plugins.Catalog, error) { return c.catalog(ctx, false) }

// catalog fetches the catalog; fresh skips the cache (update checks and updates).
func (c *Core) catalog(ctx context.Context, fresh bool) (*plugins.Catalog, error) {
	c.mkt.mu.Lock()
	if !fresh && c.mkt.catalog != nil && time.Since(c.mkt.fetchedAt) < catalogTTL {
		cat := c.mkt.catalog
		c.mkt.mu.Unlock()
		return cat, nil
	}
	c.mkt.mu.Unlock()
	cat, err := plugins.FetchCatalog(ctx, version.Version)
	if err != nil {
		return nil, err
	}
	if err := c.checkCatalogAge(cat); err != nil {
		return nil, err
	}
	c.mkt.mu.Lock()
	c.mkt.catalog, c.mkt.fetchedAt = cat, time.Now()
	c.mkt.mu.Unlock()
	return cat, nil
}

// StoreList is the marketplace as the agent and the panel see it.
func (c *Core) StoreList(ctx context.Context) (map[string]any, error) {
	cat, catErr := c.catalog(ctx, true) // what the user browses is always current
	if catErr != nil {
		cat, _ = c.Catalog(ctx) // fall back to the last good copy
	}
	records := map[string]vault.Plugin{}
	connected := map[string][]map[string]any{}
	_ = c.Vault.View(func(d *vault.Data) error {
		for id, p := range d.Plugins {
			records[id] = *p
		}
		for id, i := range d.Integrations {
			for _, a := range i.Accounts {
				connected[id] = append(connected[id], map[string]any{"id": a.ID, "label": a.Label, "default": a == i.Find(""), "agents": a.Agents, "temp_agents": a.TempAgents})
			}
		}
		return nil
	})
	var list []map[string]any
	seen := map[string]bool{}
	if cat != nil {
		for i := range cat.Plugins {
			e := &cat.Plugins[i]
			seen[e.ID] = true
			item := map[string]any{"id": e.ID, "name": e.Name, "summary": e.Summary, "publisher": e.Publisher.Name,
				"reviewed": true, "source": e.Source}
			if v, ok := e.Latest(version.Version); ok {
				item["latest"] = v.Version
			} else {
				item["requires_newer_rubi"] = true
			}
			c.describeInstalled(item, e.ID, records, connected)
			if rec, ok := records[e.ID]; ok {
				if v, ok := e.Latest(version.Version); ok && update.Newer(v.Version, rec.Version) {
					item["update_available"] = v.Version
				}
			}
			list = append(list, item)
		}
	}
	for id, rec := range records { // sideloaded plugins
		if seen[id] {
			continue
		}
		m, _ := c.Store.Get(id)
		item := map[string]any{"id": id, "name": m.Name, "summary": m.Description, "publisher": rec.PublisherName,
			"reviewed": rec.Reviewed, "source": rec.Source}
		c.describeInstalled(item, id, records, connected)
		list = append(list, item)
	}
	out := map[string]any{"plugins": list}
	if catErr != nil {
		out["catalog_error"] = catErr.Error()
	}
	return out, nil
}

func (c *Core) describeInstalled(item map[string]any, id string, records map[string]vault.Plugin, connected map[string][]map[string]any) {
	rec, ok := records[id]
	if !ok {
		item["installed"] = false
		return
	}
	item["installed"], item["version"], item["running"] = true, rec.Version, c.Runner.Running(id)
	if rec.Previous != nil {
		item["previous_version"] = rec.Previous.Version
	}
	if m, ok := c.Store.Get(id); ok {
		if len(m.Config) > 0 {
			item["has_config"] = true
		}
		if m.Panel != "" {
			item["panel"] = m.Panel
		}
		item["setup"] = len(m.Fields)+len(m.Secrets) > 0 // the plugin connects accounts
	}
	if accts := connected[id]; len(accts) > 0 {
		item["connected"], item["accounts"] = true, accts
		item["account"] = accts[0]["label"] // the default (older panels show one)
		for _, a := range accts {
			if a["default"] == true {
				item["account"] = a["label"]
			}
		}
	} else {
		item["connected"] = false
	}
}

// maxBatch is the most plugins one install, update, remove or rollback approval covers (a variable so that a test
// can lower it).
var maxBatch = 20

// RequestPluginInstall downloads and verifies a plugin, then asks the user to approve installing it.
// ref is a catalog id, or a source URL for sideloading.
func (c *Core) RequestPluginInstall(ctx context.Context, ref string) (map[string]any, error) {
	return c.RequestPluginInstalls(ctx, []string{ref})
}

// RequestPluginInstalls installs several plugins with one approval: the user sees each plugin with its
// publisher key, whether it was reviewed and what it can do, and can leave any of them out. A list of one
// is the ordinary install approval. A ref that can't be fetched or verified is reported under "failed"
// and does not hold back the others.
func (c *Core) RequestPluginInstalls(ctx context.Context, refs []string) (map[string]any, error) {
	if c.State() != Unlocked {
		return nil, errors.New("Rubi is locked")
	}
	if !c.WebhookConfigured() {
		return nil, ErrNoWebhook
	}
	refs, err := cleanList(refs, "plugin")
	if err != nil {
		return nil, err
	}
	cat, catErr := c.Catalog(ctx)
	var ready []*pluginInstall
	failed := map[string]string{}
	seen := map[string]string{} // plugin id -> the ref that brought it
	for _, ref := range refs {
		in, err := c.prepareInstall(ctx, ref, cat, catErr)
		if err != nil {
			if len(refs) == 1 {
				return nil, err
			}
			failed[ref] = err.Error()
			continue
		}
		if first, dup := seen[in.id]; dup {
			c.unstage(in.cand.Dir)
			failed[ref] = in.id + " is already in this request (" + first + ")"
			continue
		}
		seen[in.id] = ref
		ready = append(ready, in)
	}
	var res map[string]any
	switch len(ready) {
	case 0:
		return map[string]any{"status": "nothing_to_install", "failed": failed}, nil
	case 1:
		res, err = ready[0].submitSingle(ctx)
	default:
		res, err = c.submitInstalls(ctx, ready)
	}
	if err != nil {
		return nil, err
	}
	if len(failed) > 0 {
		res["failed"] = failed
	}
	return res, nil
}

// pluginInstall is a verified, staged release waiting for the user's approval to be installed.
type pluginInstall struct {
	core    *Core
	id      string
	cand    *plugins.Candidate
	label   string
	preview map[string]any
}

// prepareInstall downloads and verifies ref (a store id or a source URL); the result is staged.
func (c *Core) prepareInstall(ctx context.Context, ref string, cat *plugins.Catalog, catErr error) (*pluginInstall, error) {
	var cand *plugins.Candidate
	var err error
	if !strings.Contains(ref, "/") {
		if catErr != nil {
			return nil, catErr
		}
		e, ok := cat.Entry(ref)
		if !ok {
			return nil, fmt.Errorf("%q is not in the Rubi store; see rubi_store, or pass a source URL to sideload", ref)
		}
		if c.record(e.ID) != nil {
			return nil, fmt.Errorf("%s is already installed; use rubi_plugin_update", e.Name)
		}
		cand, err = c.fetchReviewed(ctx, e)
	} else {
		// An id that is in the store can only come from the store. Without the store's list that can't
		// be checked, so sideloading waits until it can be (a blocked store must not let an impostor in).
		if catErr != nil {
			return nil, fmt.Errorf("the Rubi store can't be reached to check this plugin; try again later: %w", catErr)
		}
		cand, err = c.fetchSideload(ctx, ref, "", cat)
		if err == nil && c.record(cand.Manifest.ID) != nil {
			c.unstage(cand.Dir)
			return nil, fmt.Errorf("plugin %s is already installed; remove it first to install it from another source", cand.Manifest.ID)
		}
	}
	if err != nil {
		return nil, err
	}
	m := cand.Manifest
	label := m.Name + " " + m.Version
	if !cand.Reviewed {
		label += " (not reviewed)"
	}
	return &pluginInstall{core: c, id: m.ID, cand: cand, label: label, preview: permissions(m, cand.Reviewed, cand.Source)}, nil
}

// apply installs the approved plugin. quiet: the user turned update notifications off for it.
func (in *pluginInstall) apply(quiet bool) error {
	c, cand := in.core, in.cand
	m := cand.Manifest
	c.mkt.applyMu.Lock()
	defer c.mkt.applyMu.Unlock()
	// Another approval may have installed this plugin since the user was asked (two requests for it waited);
	// installing over it would replace its files and record, and the user approved a first install.
	if c.record(m.ID) != nil {
		return fmt.Errorf("%s is already installed (changed since you were asked); nothing was done", m.Name)
	}
	if err := c.Store.Commit(cand, ""); err != nil {
		return err
	}
	if err := c.Vault.Update(func(d *vault.Data) error {
		d.Plugins[m.ID] = newRecord(cand)
		return nil
	}); err != nil {
		_ = c.Store.Remove(m.ID)
		return err
	}
	c.Audit.Record("plugin.installed", audit.Fields{"plugin": m.ID, "version": m.Version, "reviewed": cand.Reviewed,
		"source": cand.Source})
	_ = c.SetUpdateNotify(m.ID, !quiet) // chosen on the install screen
	c.toolsChanged()
	c.startInstalled(m.ID, m.Version, cand.Tree)
	if len(m.Fields)+len(m.Secrets) == 0 {
		go c.autoConnect(m) // nothing to enter: the install approval covers connecting it
	}
	return nil
}

// decorate adds the next step for a plugin that needs the user's setup.
func (in *pluginInstall) decorate(res map[string]any) {
	if m := in.cand.Manifest; len(m.Fields)+len(m.Secrets) > 0 {
		res["next_step"] = "Installed. Now give the user the setup link: rubi_link(\"setup:" + m.ID + "\")."
	}
}

func (in *pluginInstall) submitSingle(ctx context.Context) (map[string]any, error) {
	m := in.cand.Manifest
	return in.core.submitPluginChange(ctx, in.cand, "rubi.plugin.install", "Install "+m.Name+" "+m.Version, in.preview,
		func(option string) error { return in.apply(option == "install_quiet") }, in.decorate)
}

// installOptions are the choices of an install approval: the user picks, and signs, whether the agent hears
// about the plugin's updates.
func installOptions() (question string, options []approvals.Option) {
	return "Tell your agent when new versions come out?", []approvals.Option{
		{Key: "install_quiet", Label: "Install", Meaning: "updates wait on the Updates page"},
		{Key: "install", Label: "Install and notify about updates", Meaning: "your agent tells you about new versions"}}
}

// submitInstalls asks for one approval covering several plugins, an item each. Execute installs only the
// ticked items and reports every item.
func (c *Core) submitInstalls(ctx context.Context, ready []*pluginInstall) (map[string]any, error) {
	items := make([]approvals.Item, len(ready))
	byID := map[string]*pluginInstall{}
	unreviewed := 0
	for i, in := range ready {
		// A plugin from outside the store starts unticked: installing it takes a tick of its own, so that one tap
		// on the reviewed ones of an "install these" request can't carry it through.
		items[i] = approvals.Item{Key: in.id, Label: in.label, Preview: in.preview, DefaultOff: !in.cand.Reviewed}
		byID[in.id] = in
		if !in.cand.Reviewed {
			unreviewed++
		}
	}
	preview := map[string]any{"plugins": len(ready),
		"note": "Each plugin is verified against its signed release. Uncheck any you'd rather not install."}
	if unreviewed > 0 {
		preview["warning"] = fmt.Sprintf("Not from the Rubi store and not reviewed by Rubi-Project: %d of these plugins (marked below, and not ticked). Tick one only if you trust its publisher.", unreviewed)
	}
	unstageAll := func() {
		for _, in := range ready {
			c.unstage(in.cand.Dir)
		}
	}
	question, options := installOptions()
	res, err := c.Approvals.Submit(ctx, approvals.Request{Integration: "rubi", Kind: "rubi.plugin.install",
		Summary: fmt.Sprintf("Install %d plugins", len(ready)), Question: question, Preview: preview, Options: options, Items: items,
		Execute: func(_ context.Context, option string) (any, error) {
			defer unstageAll()
			key, chosen := approvals.SplitOption(option)
			var installed []map[string]any
			errs := map[string]string{}
			for _, id := range chosen {
				in := byID[id]
				if in == nil {
					continue
				}
				if err := in.apply(key == "install_quiet"); err != nil {
					errs[id] = err.Error()
					continue
				}
				item := map[string]any{"plugin": id, "version": in.cand.Manifest.Version}
				in.decorate(item)
				installed = append(installed, item)
			}
			result := map[string]any{"installed": installed}
			if len(errs) > 0 {
				result["failed"] = errs
			}
			if len(installed) == 0 {
				return result, errors.New("no plugin could be installed")
			}
			return result, nil
		}})
	if err != nil {
		unstageAll()
		return nil, err
	}
	if id, ok := res["approval_id"].(string); ok {
		go func() {
			for _, in := range ready {
				c.cleanupWhenDone(id, in.cand.Dir)
			}
		}()
	}
	got := make([]string, len(ready))
	for i, in := range ready {
		got[i] = in.id
	}
	res["plugins"] = got
	return res, nil
}

// cleanList trims a list of ids or refs, drops blanks and repeats, and checks the size.
func cleanList(in []string, what string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("name at least one %s", what)
	}
	if len(out) > maxBatch {
		return nil, fmt.Errorf("at most %d plugins at a time", maxBatch)
	}
	return out, nil
}

// RequestPluginUpdate verifies the newest release of an installed plugin and asks to install it.
func (c *Core) RequestPluginUpdate(ctx context.Context, id string) (map[string]any, error) {
	if c.State() != Unlocked {
		return nil, errors.New("Rubi is locked")
	}
	rec := c.record(id)
	if rec == nil {
		return nil, fmt.Errorf("plugin %q is not installed", id)
	}
	// Other plugins with updates go into the same approval (the user can untick any), so a Bot asking
	// plugin by plugin still gives the user one link.
	var others []string
	_ = c.Vault.View(func(d *vault.Data) error {
		for other := range d.Plugins {
			if other != id {
				others = append(others, other)
			}
		}
		return nil
	})
	sort.Strings(others)
	ids := []string{id}
	for _, other := range others {
		if len(ids) == 20 {
			break
		}
		if ok, err := c.updateAvailable(ctx, other); ok && err == nil && !c.QuietUpdates(other) {
			ids = append(ids, other)
		}
	}
	res, err := c.requestUpdates(ctx, ids, map[string]bool{id: true})
	if err != nil || res["status"] != "nothing_to_update" {
		return res, err
	}
	// Nothing to approve: answer about the plugin the Bot named, as a single request always did.
	if failed, _ := res["failed"].(map[string]string); failed[id] != "" {
		return nil, errors.New(failed[id])
	}
	return map[string]any{"status": "up_to_date", "plugin": id, "version": rec.Version}, nil
}

// pluginUpdate is a verified release waiting for the user's approval.
type pluginUpdate struct {
	core           *Core
	id, name, from string
	cand           *plugins.Candidate
	summary        string
	preview        map[string]any
	pinnedKey      string           // the publisher key recorded at install (sideloaded plugins)
	old            plugins.Manifest // the installed release's manifest
}

// powerFields are the parts of a manifest that confer power or decide what the user is asked, so that a change to
// any of them is a different decision from the one made at install (for a plugin from outside the store):
//   - entry: the executable that runs;
//   - fields, secrets: what the plugin asks the user to enter and keep (secrets: also internal and renewable ones);
//   - actions: what it can do and at which approval level, including a lowered level and a lock removed;
//   - events: what it can wake the agent about (and which fields are untrusted);
//   - egress: the hosts it may reach;
//   - tools: the tools it hands the agent;
//   - panel: a page of its own in the panel, acting for the user;
//   - config: the settings the user is shown, their defaults and per-account scope (a changed default changes
//     what the plugin does with no one deciding);
//   - hooks: private web addresses that reach it;
//   - home: what it does through Rubi Home (for example claude-start).
//
// The rest (name, description, needs, version, source, min_rubi, the publisher's name and URL) confers none; the
// publisher key is checked on its own.
var powerFields = []struct {
	name string
	get  func(m plugins.Manifest) any
}{
	{"entry", func(m plugins.Manifest) any { return m.Entry }},
	{"fields", func(m plugins.Manifest) any { return m.Fields }},
	{"secrets", func(m plugins.Manifest) any { return m.Secrets }},
	{"actions", func(m plugins.Manifest) any { return m.Actions }},
	{"events", func(m plugins.Manifest) any { return m.Events }},
	{"egress", func(m plugins.Manifest) any { return m.Egress }},
	{"tools", func(m plugins.Manifest) any { return m.Tools }},
	{"panel", func(m plugins.Manifest) any { return m.Panel }},
	{"config", func(m plugins.Manifest) any { return m.Config }},
	{"hooks", func(m plugins.Manifest) any { return m.Hooks }},
	{"home", func(m plugins.Manifest) any { return m.Home }},
}

// changedPowerFields names the permission-bearing parts of the manifest that differ between two releases.
func changedPowerFields(old, m plugins.Manifest) []string {
	var changed []string
	for _, f := range powerFields {
		a, _ := json.Marshal(f.get(old))
		b, _ := json.Marshal(f.get(m))
		if string(a) != string(b) {
			changed = append(changed, f.name)
		}
	}
	return changed
}

// startsUnticked says whether this update must be ticked on purpose in "update everything", and why. A plugin
// from the store is covered by the review. One from outside it is ticked by default only when the release is
// signed by the key pinned at install and nothing in the manifest that confers power changed (powerFields):
// anything else is a different trust decision than the one the user made when installing it.
func (u *pluginUpdate) startsUnticked() (bool, string) {
	if u.cand.Reviewed {
		return false, ""
	}
	if u.pinnedKey == "" || strings.TrimSpace(u.cand.Manifest.Publisher.Key) != strings.TrimSpace(u.pinnedKey) {
		return true, "publisher key changed"
	}
	changed := changedPowerFields(u.old, u.cand.Manifest)
	if len(changed) == 0 {
		return false, ""
	}
	if added := newPermissions(u.old, u.cand.Manifest); added != "" {
		return true, "new permissions: " + added + " (changed: " + strings.Join(changed, ", ") + ")"
	}
	return true, "it changes what the plugin may do (changed: " + strings.Join(changed, ", ") + ")"
}

// preparePluginUpdate downloads and verifies the newest release of id. done is set (and u nil) when
// there is nothing to approve, e.g. the plugin is up to date.
func (c *Core) preparePluginUpdate(ctx context.Context, id string) (u *pluginUpdate, done map[string]any, err error) {
	rec := c.record(id)
	old, _ := c.Store.Get(id)
	if rec == nil {
		return nil, nil, fmt.Errorf("plugin %q is not installed", id)
	}
	upToDate := map[string]any{"status": "up_to_date", "plugin": id, "version": rec.Version}
	var cand *plugins.Candidate
	if rec.Reviewed {
		cat, cerr := c.catalog(ctx, true)
		if cerr != nil {
			return nil, nil, cerr
		}
		e, ok := cat.Entry(id)
		if !ok {
			return nil, nil, fmt.Errorf("%s is no longer in the Rubi store", old.Name)
		}
		v, ok := e.Latest(version.Version)
		if !ok || !update.Newer(v.Version, rec.Version) {
			return nil, upToDate, nil
		}
		cand, err = c.fetchReviewed(ctx, e)
	} else {
		latest, lerr := c.sideloadLatest(ctx, rec.Source)
		if lerr != nil {
			return nil, nil, lerr
		}
		if !update.Newer(latest, rec.Version) {
			return nil, upToDate, nil
		}
		cand, err = c.fetchSideload(ctx, rec.Source, rec.PublisherKey, nil)
	}
	if err != nil {
		return nil, nil, err
	}
	m := cand.Manifest
	if !update.Newer(m.Version, rec.Version) { // never an older (or the same) release as an "update"
		c.unstage(cand.Dir)
		return nil, upToDate, nil
	}
	if m.ID != id {
		c.unstage(cand.Dir)
		return nil, nil, errors.New("the source now publishes a different plugin; refusing to update")
	}
	preview := permissions(m, cand.Reviewed, cand.Source)
	preview["current"] = rec.Version
	if added := newPermissions(old, m); added != "" {
		preview["new_permissions"] = added
	}
	return &pluginUpdate{core: c, id: id, name: m.Name, from: rec.Version, cand: cand, preview: preview, pinnedKey: rec.PublisherKey, old: old,
		summary: "Update " + m.Name + " to " + m.Version}, nil, nil
}

// apply installs the approved release, keeping the replaced version for rollback.
func (c *Core) applyPluginUpdate(u *pluginUpdate) error {
	id, m, cand := u.id, u.cand.Manifest, u.cand
	c.mkt.applyMu.Lock()
	defer c.mkt.applyMu.Unlock()
	// The user approved going from u.from to this version. If the plugin is no longer at u.from (another
	// approval, a rollback or a reinstall got there first), or this is not newer than what is installed, the
	// approval is stale: applying it would install an older release over a newer one and make the rollback
	// target wrong.
	if rec := c.record(id); rec == nil || rec.Version != u.from || !update.Newer(m.Version, rec.Version) {
		return fmt.Errorf("%s changed since you were asked; nothing was done", u.name)
	}
	c.Runner.Stop(id)
	if err := c.Store.Commit(cand, u.from); err != nil { // keep the old version for rollback
		return err
	}
	if err := c.Vault.Update(func(d *vault.Data) error {
		r := newRecord(cand)
		if prev := d.Plugins[id]; prev != nil {
			r.InstalledAt = prev.InstalledAt
			old := prev.Current()
			r.Previous = &old
		}
		d.Plugins[id] = r
		return nil
	}); err != nil {
		return err
	}
	c.Audit.Record("plugin.updated", audit.Fields{"plugin": id, "from": u.from, "to": m.Version})
	c.toolsChanged()
	c.startInstalled(id, m.Version, cand.Tree)
	return nil
}

func (u *pluginUpdate) apply() error { return u.core.applyPluginUpdate(u) }

// RequestPluginUpdates updates several plugins with one approval: the user sees every plugin with its
// permissions and can leave any of them out. No ids means every installed plugin with an update. A single
// update is an ordinary update approval.
func (c *Core) RequestPluginUpdates(ctx context.Context, ids []string) (map[string]any, error) {
	explicit := map[string]bool{}
	for _, id := range ids {
		explicit[id] = true
	}
	return c.requestUpdates(ctx, ids, explicit)
}

// requestUpdates asks the user to approve updating ids (explicit: the ones the Bot named). One
// plugin-update approval waits at a time: one that already covers these plugins is the answer; one that
// doesn't is replaced by an approval for all of them, once that is ready.
func (c *Core) requestUpdates(ctx context.Context, ids []string, explicit map[string]bool) (map[string]any, error) {
	if c.State() != Unlocked {
		return nil, errors.New("Rubi is locked")
	}
	if len(ids) == 0 {
		_ = c.Vault.View(func(d *vault.Data) error {
			for id := range d.Plugins {
				ids = append(ids, id)
			}
			return nil
		})
		sort.Strings(ids)
	}
	if len(explicit) > 20 {
		return nil, errors.New("at most 20 plugins at a time")
	}
	c.mkt.updMu.Lock()
	defer c.mkt.updMu.Unlock()
	var old *pendingUpdate
	if p := c.mkt.pendingUpdate; p != nil {
		if snap, err := c.Approvals.Get(p.approvalID); err != nil || snap.State != approvals.Pending || time.Until(snap.ExpiresAt) < 2*time.Minute {
			c.mkt.pendingUpdate = nil
		} else {
			covered := true
			for _, id := range ids {
				if p.ids[id] {
					continue
				}
				// A plugin the Bot named that can't be checked isn't covered: the normal path reports why.
				if ok, err := c.updateAvailable(ctx, id); ok || err != nil && explicit[id] {
					covered = false
				}
			}
			if covered {
				return p.reply("These updates already wait for the user's approval at this link; send it if you haven't."), nil
			}
			old = p
			have := map[string]bool{}
			for _, id := range ids {
				have[id] = true
			}
			for id := range p.ids {
				if !have[id] {
					ids = append(ids, id)
				}
			}
		}
	}
	if len(ids) > 20 { // keep what the Bot named and what was already waiting, then others while there's room
		keep := func(id string) bool { return explicit[id] || old != nil && old.ids[id] }
		var kept []string
		for _, id := range ids {
			if keep(id) {
				kept = append(kept, id)
			}
		}
		for _, id := range ids {
			if len(kept) < 20 && !keep(id) {
				kept = append(kept, id)
			}
		}
		ids = kept
	}
	res, err := c.submitUpdates(ctx, ids)
	if err == nil && res["approval_id"] != nil {
		c.cancelPendingAll() // the plugin update asked for now replaces a waiting "update everything" (one slot)
	}
	if old == nil {
		if err == nil {
			c.rememberUpdate(res)
		}
		return res, err
	}
	// Replace the waiting approval only once its successor exists, and only if the user hasn't just
	// approved it.
	if err != nil || res["approval_id"] == nil {
		return old.reply("These updates still wait at this link; the others couldn't be added."), nil
	}
	if snap, _ := c.Approvals.Cancel(old.approvalID); snap.State != approvals.Cancelled {
		if id, _ := res["approval_id"].(string); id != "" {
			_, _ = c.Approvals.Cancel(id)
		}
		return old.reply("The user has just decided the earlier update link; ask again later for anything it didn't cover."), nil
	}
	c.rememberUpdate(res)
	res["note"] = "This one link now covers all the updates; the earlier update link no longer works."
	return res, nil
}

// prepareUpdates downloads and verifies the newest release of each of ids. The plugins with an update are
// returned (staged); out reports the ones that are up to date or could not be checked.
func (c *Core) prepareUpdates(ctx context.Context, ids []string) (ready []*pluginUpdate, out map[string]any) {
	var upToDate []string
	failed := map[string]string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		u, done, err := c.preparePluginUpdate(ctx, id)
		switch {
		case err != nil:
			failed[id] = err.Error()
		case done != nil:
			upToDate = append(upToDate, id)
		default:
			ready = append(ready, u)
		}
	}
	out = map[string]any{}
	if len(upToDate) > 0 {
		out["up_to_date"] = upToDate
	}
	if len(failed) > 0 {
		out["failed"] = failed
	}
	return ready, out
}

// runPluginUpdates applies the chosen plugin updates one by one; a failure of one does not stop the others.
func runPluginUpdates(chosen []string, byID map[string]*pluginUpdate) (updated []map[string]any, errs map[string]string) {
	errs = map[string]string{}
	for _, id := range chosen {
		u := byID[id]
		if u == nil {
			continue
		}
		if err := u.apply(); err != nil {
			errs[id] = err.Error()
			continue
		}
		updated = append(updated, map[string]any{"plugin": id, "version": u.cand.Manifest.Version})
	}
	return updated, errs
}

// submitUpdates prepares the updates of ids and submits one approval for those that have one.
func (c *Core) submitUpdates(ctx context.Context, ids []string) (map[string]any, error) {
	ready, out := c.prepareUpdates(ctx, ids)
	switch len(ready) {
	case 0:
		out["status"] = "nothing_to_update"
		return out, nil
	case 1:
		u := ready[0]
		res, err := c.submitPluginChange(ctx, u.cand, "rubi.plugin.update", u.summary, u.preview,
			func(string) error { return u.apply() }, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range out {
			res[k] = v
		}
		res["plugins"] = []string{u.id}
		return res, nil
	}

	items := make([]approvals.Item, len(ready))
	byID := map[string]*pluginUpdate{}
	for i, u := range ready {
		items[i] = approvals.Item{Key: u.id, Label: u.name + " " + u.from + " → " + u.cand.Manifest.Version, Preview: u.preview}
		byID[u.id] = u
	}
	c.mkt.mu.Lock()
	if c.mkt.staged == nil {
		c.mkt.staged = map[string]bool{}
	}
	for _, u := range ready {
		c.mkt.staged[u.cand.Dir] = true
	}
	c.mkt.mu.Unlock()
	unstageAll := func() {
		for _, u := range ready {
			c.unstage(u.cand.Dir)
		}
	}
	res, err := c.Approvals.Submit(ctx, approvals.Request{Integration: "rubi", Kind: "rubi.plugin.update",
		Summary: fmt.Sprintf("Update %d plugins", len(ready)),
		Preview: map[string]any{"plugins": len(ready), "note": "Each plugin is verified against its signed release. Uncheck any you'd rather keep as it is."},
		Options: []approvals.Option{{Key: "install", Label: "Update"}}, Items: items,
		Execute: func(_ context.Context, option string) (any, error) {
			defer unstageAll()
			_, chosen := approvals.SplitOption(option)
			updated, errs := runPluginUpdates(chosen, byID)
			result := map[string]any{"updated": updated}
			if len(errs) > 0 {
				result["failed"] = errs
			}
			if len(updated) == 0 {
				return result, errors.New("no plugin could be updated")
			}
			return result, nil
		}})
	if err != nil {
		unstageAll()
		return nil, err
	}
	if id, ok := res["approval_id"].(string); ok {
		go func() {
			for _, u := range ready {
				c.cleanupWhenDone(id, u.cand.Dir)
			}
		}()
	}
	for k, v := range out {
		res[k] = v
	}
	got := make([]string, len(ready))
	for i, u := range ready {
		got[i] = u.id
	}
	res["plugins"] = got
	return res, nil
}

// rememberUpdate keeps the plugin-update approval that now waits, so later requests reuse its link.
func (c *Core) rememberUpdate(res map[string]any) {
	id, _ := res["approval_id"].(string)
	if id == "" {
		return
	}
	p := &pendingUpdate{approvalID: id, ids: map[string]bool{}, res: map[string]any{}}
	if list, ok := res["plugins"].([]string); ok {
		for _, pid := range list {
			p.ids[pid] = true
		}
	}
	for k, v := range res {
		p.res[k] = v
	}
	c.mkt.pendingUpdate = p
}

// reply answers with the waiting approval's link.
func (p *pendingUpdate) reply(note string) map[string]any {
	out := map[string]any{}
	for k, v := range p.res {
		out[k] = v
	}
	out["note"] = note
	return out
}

// updateAvailable reports whether the store or the plugin's source has a newer version than installed.
func (c *Core) updateAvailable(ctx context.Context, id string) (bool, error) {
	rec := c.record(id)
	if rec == nil {
		return false, fmt.Errorf("plugin %q is not installed", id)
	}
	if !rec.Reviewed {
		v, err := c.sideloadLatest(ctx, rec.Source)
		if err != nil {
			return false, err
		}
		return update.Newer(v, rec.Version), nil
	}
	cat, err := c.catalog(ctx, false)
	if err != nil {
		return false, err
	}
	e, ok := cat.Entry(id)
	if !ok {
		return false, fmt.Errorf("%s is no longer in the Rubi store", id)
	}
	v, ok := e.Latest(version.Version)
	return ok && update.Newer(v.Version, rec.Version), nil
}

// pluginOp is one plugin's part of a remove or rollback request: how it reads on the approval screen and
// what approving it does.
type pluginOp struct {
	id      string
	label   string         // the item's title in a batch
	summary string         // the approval's title when this is the only one
	preview map[string]any // what the user sees for it
	run     func() (map[string]any, error)
}

// opRequest describes a kind of request that may cover several plugins.
type opRequest struct {
	kind     string
	option   approvals.Option
	prepare  func(id string) (*pluginOp, error)
	doneKey  string // the list of what was done in a batch's result
	nothing  string // status when no plugin can be handled
	noneDone string // the error when every ticked plugin failed
	note     string // the batch's own line on the approval screen
}

// requestOps asks for one approval for the plugins in ids: an ordinary approval for one, a batch with an
// item per plugin for several (the user can untick any; a plugin that can't be handled is reported under
// "failed" and does not hold back the others).
func (c *Core) requestOps(ctx context.Context, ids []string, r opRequest) (map[string]any, error) {
	if c.State() != Unlocked {
		return nil, errors.New("Rubi is locked")
	}
	ids, err := cleanList(ids, "plugin")
	if err != nil {
		return nil, err
	}
	var ops []*pluginOp
	failed := map[string]string{}
	for _, id := range ids {
		op, err := r.prepare(id)
		if err != nil {
			if len(ids) == 1 {
				return nil, err
			}
			failed[id] = err.Error()
			continue
		}
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		return map[string]any{"status": r.nothing, "failed": failed}, nil
	}
	var res map[string]any
	if len(ops) == 1 {
		op := ops[0]
		res, err = c.Approvals.Submit(ctx, approvals.Request{Integration: "rubi", Kind: r.kind, Summary: op.summary,
			Preview: op.preview, Options: []approvals.Option{r.option},
			Execute: func(context.Context, string) (any, error) { return op.run() }})
	} else {
		items := make([]approvals.Item, len(ops))
		byID := map[string]*pluginOp{}
		for i, op := range ops {
			items[i] = approvals.Item{Key: op.id, Label: op.label, Preview: op.preview}
			byID[op.id] = op
		}
		res, err = c.Approvals.Submit(ctx, approvals.Request{Integration: "rubi", Kind: r.kind,
			Summary: fmt.Sprintf("%s %d plugins", r.option.Label, len(ops)),
			Preview: map[string]any{"plugins": len(ops), "note": r.note},
			Options: []approvals.Option{r.option}, Items: items,
			Execute: func(_ context.Context, option string) (any, error) {
				_, chosen := approvals.SplitOption(option)
				var done []map[string]any
				errs := map[string]string{}
				for _, id := range chosen {
					op := byID[id]
					if op == nil {
						continue
					}
					out, err := op.run()
					if err != nil {
						errs[id] = err.Error()
						continue
					}
					done = append(done, out)
				}
				result := map[string]any{r.doneKey: done}
				if len(errs) > 0 {
					result["failed"] = errs
				}
				if len(done) == 0 {
					return result, errors.New(r.noneDone)
				}
				return result, nil
			}})
	}
	if err != nil {
		return nil, err
	}
	if len(failed) > 0 {
		res["failed"] = failed
	}
	got := make([]string, len(ops))
	for i, op := range ops {
		got[i] = op.id
	}
	res["plugins"] = got
	return res, nil
}

// RequestPluginRollback asks to switch a plugin back to the version it replaced. Doing it again switches
// forward again. Settings, secrets and state stay.
func (c *Core) RequestPluginRollback(ctx context.Context, id string) (map[string]any, error) {
	return c.RequestPluginRollbacks(ctx, []string{id})
}

// RequestPluginRollbacks switches several plugins back with one approval (for example after one update of
// everything broke more than one), an item each.
func (c *Core) RequestPluginRollbacks(ctx context.Context, ids []string) (map[string]any, error) {
	return c.requestOps(ctx, ids, opRequest{kind: "rubi.plugin.rollback", option: approvals.Option{Key: "rollback", Label: "Roll back"},
		prepare: c.prepareRollback, doneKey: "rolled_back", nothing: "nothing_to_roll_back", noneDone: "no plugin could be rolled back",
		note: "Settings, passwords and state stay. Uncheck any you'd rather keep as it is."})
}

func (c *Core) prepareRollback(id string) (*pluginOp, error) {
	rec := c.record(id)
	m, _ := c.Store.Get(id)
	if rec == nil {
		return nil, fmt.Errorf("plugin %q is not installed", id)
	}
	if rec.Previous == nil {
		return nil, fmt.Errorf("%s has no earlier version to go back to", m.Name)
	}
	prev := *rec.Previous
	if err := c.Store.Verify(id, prev.Version, prev.Tree); err != nil {
		return nil, fmt.Errorf("can't roll back: %w", err)
	}
	return &pluginOp{id: id, label: m.Name + " " + rec.Version + " → " + prev.Version,
		summary: "Roll " + m.Name + " back to " + prev.Version,
		preview: map[string]any{"plugin": m.Name, "current": rec.Version, "back_to": prev.Version,
			"effect": "Settings, passwords and state stay. You can switch forward again the same way."},
		run: func() (map[string]any, error) {
			c.Runner.Stop(id)
			if err := c.Store.Verify(id, prev.Version, prev.Tree); err != nil {
				c.startInstalled(id, rec.Version, rec.Tree)
				return nil, err
			}
			if err := c.Store.Activate(id, prev.Version); err != nil {
				c.startInstalled(id, rec.Version, rec.Tree)
				return nil, err
			}
			if err := c.Vault.Update(func(d *vault.Data) error {
				r := d.Plugins[id]
				if r == nil {
					return errors.New("plugin record missing")
				}
				cur := r.Current()
				r.Use(prev)
				r.Previous = &cur
				return nil
			}); err != nil {
				_ = c.Store.Activate(id, rec.Version)
				c.startInstalled(id, rec.Version, rec.Tree)
				return nil, err
			}
			c.Audit.Record("plugin.rolled_back", audit.Fields{"plugin": id, "from": rec.Version, "to": prev.Version})
			c.toolsChanged()
			c.startInstalled(id, prev.Version, prev.Tree)
			return map[string]any{"plugin": id, "version": prev.Version, "previous": rec.Version}, nil
		}}, nil
}

// RequestPluginRemove asks to uninstall a plugin and erase everything it stored.
func (c *Core) RequestPluginRemove(ctx context.Context, id string) (map[string]any, error) {
	return c.RequestPluginRemoves(ctx, []string{id})
}

// RequestPluginRemoves uninstalls several plugins with one approval, an item each.
func (c *Core) RequestPluginRemoves(ctx context.Context, ids []string) (map[string]any, error) {
	return c.requestOps(ctx, ids, opRequest{kind: "rubi.plugin.remove", option: approvals.Option{Key: "remove", Label: "Remove"},
		prepare: c.prepareRemove, doneKey: "removed", nothing: "nothing_to_remove", noneDone: "no plugin could be removed",
		note: "Each is stopped and deleted, with its stored passwords, settings and state. Uncheck any you'd rather keep."})
}

func (c *Core) prepareRemove(id string) (*pluginOp, error) {
	m, ok := c.Store.Get(id)
	rec := c.record(id)
	if !ok && rec == nil {
		return nil, fmt.Errorf("plugin %q is not installed", id)
	}
	name := m.Name
	if name == "" {
		name = id
	}
	return &pluginOp{id: id, label: name, summary: "Remove " + name,
		preview: map[string]any{"plugin": name, "effect": "Stops it and deletes it, with its stored passwords, settings and state."},
		run: func() (map[string]any, error) {
			c.Runner.Stop(id)
			if err := c.Vault.Update(func(d *vault.Data) error {
				delete(d.Plugins, id)
				delete(d.Integrations, id)
				dropHooks(d, id, "")
				for kind := range d.Policy {
					if strings.HasPrefix(kind, id+".") {
						delete(d.Policy, kind)
					}
				}
				return nil
			}); err != nil {
				return nil, err
			}
			err := c.Store.Remove(id)
			c.Audit.Record("plugin.removed", audit.Fields{"plugin": id})
			c.toolsChanged()
			if err != nil {
				return nil, err
			}
			return map[string]any{"removed": id}, nil
		}}, nil
}

// submitPluginChange wraps an install or update in a strong approval and cleans up if it isn't approved.
func (c *Core) submitPluginChange(ctx context.Context, cand *plugins.Candidate, kind, summary string,
	preview map[string]any, apply func(option string) error, decorate func(map[string]any)) (map[string]any, error) {
	c.mkt.mu.Lock()
	if c.mkt.staged == nil {
		c.mkt.staged = map[string]bool{}
	}
	c.mkt.staged[cand.Dir] = true
	c.mkt.mu.Unlock()
	options := []approvals.Option{{Key: "install", Label: "Update"}}
	question := ""
	if kind == "rubi.plugin.install" {
		question, options = installOptions()
	}
	res, err := c.Approvals.Submit(ctx, approvals.Request{Integration: "rubi", Kind: kind, Summary: summary,
		Question: question, Preview: preview, Options: options,
		Execute: func(_ context.Context, option string) (any, error) {
			defer c.unstage(cand.Dir)
			if err := apply(option); err != nil {
				return nil, err
			}
			out := map[string]any{"plugin": cand.Manifest.ID, "version": cand.Manifest.Version}
			if decorate != nil {
				decorate(out)
			}
			return out, nil
		}})
	if err != nil {
		c.unstage(cand.Dir)
		return nil, err
	}
	if id, ok := res["approval_id"].(string); ok {
		go c.cleanupWhenDone(id, cand.Dir)
	}
	return res, nil
}

// startInstalled starts a plugin right after it was installed; a failure is reported, not fatal.
func (c *Core) startInstalled(id, ver, tree string) {
	if err := c.Runner.Start(id, ver, tree); err != nil {
		c.pluginProblem(id, "plugin.failed", err.Error())
	}
}

// stage marks a downloaded release as waiting, so that the next download's cleanup of old staging
// directories (several plugins in one request) leaves it alone.
func (c *Core) stage(dir string) {
	c.mkt.mu.Lock()
	if c.mkt.staged == nil {
		c.mkt.staged = map[string]bool{}
	}
	c.mkt.staged[dir] = true
	c.mkt.mu.Unlock()
}

func (c *Core) unstage(dir string) {
	c.mkt.mu.Lock()
	delete(c.mkt.staged, dir)
	c.mkt.mu.Unlock()
	_ = os.RemoveAll(dir)
}

// stagingGrace is how long past its expiry a staged download is kept when its approval has not been seen to end.
const stagingGrace = time.Hour

// stagingDeadline is the latest a staged download is kept for an approval: its expiry plus a grace, so a download
// lives at least as long as the approval can be pending (the 15 minutes of the engine, whatever it is set to).
func stagingDeadline(s approvals.Snapshot) time.Time { return s.ExpiresAt.Add(stagingGrace) }

// cleanupWhenDone deletes a staged download once its approval has ended (approved, denied, expired, cancelled).
// It does not stop earlier on a clock of its own: an approval that is being executed keeps its files.
func (c *Core) cleanupWhenDone(approvalID, dir string) {
	snap, err := c.Approvals.Get(approvalID)
	if err != nil {
		c.unstage(dir)
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), stagingDeadline(snap))
	defer cancel()
	for ctx.Err() == nil {
		snap, err := c.Approvals.Wait(ctx, approvalID, 25*time.Second)
		if err != nil || (snap.State != approvals.Pending && snap.State != approvals.Executing) {
			break
		}
	}
	c.unstage(dir)
}

func (c *Core) fetchReviewed(ctx context.Context, e *plugins.CatalogEntry) (*plugins.Candidate, error) {
	v, ok := e.Latest(version.Version)
	if !ok {
		return nil, fmt.Errorf("%s needs a newer Rubi; update Rubi first (rubi_update)", e.Name)
	}
	base, err := plugins.ReleaseBase(e.Source, v.Version)
	if err != nil {
		return nil, err
	}
	c.mkt.dlMu.Lock()
	defer c.mkt.dlMu.Unlock()
	c.cleanStaging()
	cand, err := plugins.Download(ctx, base, c.Store.Staging(), plugins.Expect{ID: e.ID, PublisherKey: e.Publisher.Key,
		SumsSHA256: v.SumsSHA256})
	if err != nil {
		return nil, err
	}
	c.stage(cand.Dir) // before anything else can clean the folder
	if cand.Manifest.Version != v.Version {
		c.unstage(cand.Dir)
		return nil, errors.New("the release doesn't match the reviewed version")
	}
	cand.Reviewed, cand.Source = true, plugins.NormalizeSource(e.Source)
	return cand, nil
}

// fetchSideload downloads a plugin from a source outside the store. key pins the publisher on updates.
func (c *Core) fetchSideload(ctx context.Context, source, key string, cat *plugins.Catalog) (*plugins.Candidate, error) {
	source = plugins.NormalizeSource(source)
	latest, err := plugins.ReleaseBase(source, "")
	if err != nil {
		return nil, err
	}
	m, err := plugins.ReadManifest(ctx, latest)
	if err != nil {
		return nil, err
	}
	if _, inStore := cat.Entry(m.ID); inStore {
		return nil, fmt.Errorf("%s is in the Rubi store; install the reviewed version: rubi_plugin_install(%q)", m.Name, m.ID)
	}
	if !update.Compatible(version.Version, m.MinRubi) {
		return nil, fmt.Errorf("%s needs Rubi %s or newer; update Rubi first (rubi_update)", m.Name, m.MinRubi)
	}
	base, err := plugins.ReleaseBase(source, m.Version) // pin the exact release, not "latest"
	if err != nil {
		return nil, err
	}
	c.mkt.dlMu.Lock()
	defer c.mkt.dlMu.Unlock()
	c.cleanStaging()
	cand, err := plugins.Download(ctx, base, c.Store.Staging(), plugins.Expect{ID: m.ID, PublisherKey: key})
	if err != nil {
		return nil, err
	}
	c.stage(cand.Dir)                       // before anything else can clean the folder
	if cand.Manifest.Version != m.Version { // the pinned release must be the version it said it was
		c.unstage(cand.Dir)
		return nil, errors.New("the source served a different version than it announced; refusing to install")
	}
	cand.Reviewed, cand.Source = false, source
	return cand, nil
}

func (c *Core) sideloadLatest(ctx context.Context, source string) (string, error) {
	base, err := plugins.ReleaseBase(source, "")
	if err != nil {
		return "", err
	}
	m, err := plugins.ReadManifest(ctx, base)
	if err != nil {
		return "", err
	}
	return m.Version, nil
}

func (c *Core) cleanStaging() {
	c.mkt.mu.Lock()
	keep := map[string]bool{}
	for d := range c.mkt.staged {
		keep[d] = true
	}
	c.mkt.mu.Unlock()
	c.Store.CleanStaging(keep)
}

func newRecord(cand *plugins.Candidate) *vault.Plugin {
	m := cand.Manifest
	return &vault.Plugin{Version: m.Version, Source: cand.Source, Reviewed: cand.Reviewed,
		PublisherName: m.Publisher.Name, PublisherKey: m.Publisher.Key, SumsSHA256: cand.SumsSHA256,
		Tree: cand.Tree, InstalledAt: time.Now().UTC()}
}

var levelNames = map[rubiplugin.Level]string{rubiplugin.None: "no approval", rubiplugin.Chat: "buttons in chat",
	rubiplugin.Tap: "Passkey tap", rubiplugin.Strong: "Passkey / password"}

// permissions describes what a plugin gets, for the install and update approval screens.
func permissions(m plugins.Manifest, reviewed bool, source string) map[string]any {
	p := map[string]any{"plugin": m.Name, "about": m.Description, "new_version": m.Version,
		"publisher": m.Publisher.Name + " · key " + plugins.Fingerprint(m.Publisher.Key)}
	if m.Publisher.URL != "" {
		p["website"] = m.Publisher.URL
	}
	if !reviewed {
		p["source"] = source
	} else if m.Source != "" {
		p["source"] = m.Source
	}
	if reviewed {
		p["review"] = "Reviewed by Rubi-Project"
	} else {
		p["review"] = "Not reviewed by Rubi-Project"
		p["warning"] = "This plugin is not from the Rubi store. Install it only if you trust its publisher: it will run on your agent's computer with access to what you enter for it."
	}
	var asks []string
	for _, f := range m.Fields {
		asks = append(asks, f.Label)
	}
	for _, s := range m.Secrets {
		if !s.Internal {
			asks = append(asks, s.Label)
		}
	}
	if len(asks) > 0 {
		p["will_ask_for"] = strings.Join(asks, ", ")
	}
	var can []string
	for _, a := range m.Actions {
		can = append(can, a.Title+" ("+levelNames[a.DefaultLevel]+")")
	}
	if len(can) > 0 {
		p["can"] = strings.Join(can, "; ")
	}
	if len(m.Egress) > 0 {
		p["connects_to"] = strings.Join(m.Egress, ", ")
	}
	if len(m.Home) > 0 {
		var what []string
		for _, h := range m.Home {
			if n, ok := homeCapabilities[h]; ok {
				what = append(what, n)
			}
		}
		p["rubi_home"] = "Uses your Rubi Home computer for: " + strings.Join(what, ", ")
	}
	if m.Hooks {
		p["web_addresses"] = "Can give you private web addresses (for example for iPhone Shortcuts) that pass requests to it"
	}
	var evs []string
	for _, e := range m.Events {
		evs = append(evs, e.Type)
	}
	if len(evs) > 0 {
		p["can_notify_about"] = strings.Join(evs, ", ")
	}
	return p
}

// newPermissions lists what an update adds compared to the installed manifest.
func newPermissions(old, m plugins.Manifest) string {
	var added []string
	has := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	var oldSecrets, oldEgress, oldEvents []string
	for _, s := range old.Secrets {
		oldSecrets = append(oldSecrets, s.Key)
	}
	oldEgress = old.Egress
	for _, e := range old.Events {
		oldEvents = append(oldEvents, e.Type)
	}
	for _, s := range m.Secrets {
		if !has(oldSecrets, s.Key) {
			added = append(added, "asks for "+s.Label)
		}
	}
	for _, a := range m.Actions {
		prev, ok := old.Action(a.Kind)
		if !ok {
			added = append(added, "can "+strings.ToLower(a.Title)+" ("+levelNames[a.DefaultLevel]+")")
		} else if levelRank(a.DefaultLevel) < levelRank(prev.DefaultLevel) {
			added = append(added, strings.ToLower(a.Title)+" now defaults to "+levelNames[a.DefaultLevel])
		}
	}
	for _, e := range m.Egress {
		if !has(oldEgress, e) {
			added = append(added, "connects to "+e)
		}
	}
	for _, e := range m.Events {
		if !has(oldEvents, e.Type) {
			added = append(added, "can notify about "+e.Type)
		}
	}
	for _, h := range m.Home {
		if !has(old.Home, h) {
			if n, ok := homeCapabilities[h]; ok {
				added = append(added, "uses Rubi Home for "+n)
			} else {
				added = append(added, "uses Rubi Home for "+h)
			}
		}
	}
	if m.Hooks && !old.Hooks {
		added = append(added, "can give private web addresses that reach it")
	}
	oldTools := map[string]bool{}
	for _, t := range old.Tools {
		oldTools[t.Name] = true
	}
	for _, t := range m.Tools {
		if !oldTools[t.Name] {
			added = append(added, "new tool "+t.Name)
		}
	}
	if old.Publisher.Key != "" && old.Publisher.Key != m.Publisher.Key {
		added = append(added, "new publisher key "+plugins.Fingerprint(m.Publisher.Key))
	}
	return strings.Join(added, "; ")
}

func levelRank(l rubiplugin.Level) int {
	return map[rubiplugin.Level]int{rubiplugin.None: 0, rubiplugin.Chat: 1, rubiplugin.Tap: 2, rubiplugin.Strong: 3}[l]
}

// CheckPluginUpdates looks for new plugin versions and tells the agent once per version.
func (c *Core) CheckPluginUpdates(ctx context.Context) {
	if c.State() != Unlocked {
		return
	}
	records := map[string]vault.Plugin{}
	_ = c.Vault.View(func(d *vault.Data) error {
		for id, p := range d.Plugins {
			records[id] = *p
		}
		return nil
	})
	if len(records) == 0 {
		return
	}
	cat, _ := c.catalog(ctx, true)
	var found []map[string]any
	for id, rec := range records {
		latest, notes := "", ""
		if rec.Reviewed {
			if e, ok := cat.Entry(id); ok {
				if v, ok := e.Latest(version.Version); ok {
					latest = v.Version
					notes = plugins.NormalizeSource(e.Source) + "/releases/tag/" + v.Version
				}
			}
		} else if v, err := c.sideloadLatest(ctx, rec.Source); err == nil {
			latest = v
		}
		if latest == "" || !update.Newer(latest, rec.Version) || rec.Notified == latest || c.QuietUpdates(id) {
			continue // quiet ones wait on the Updates page (and are announced if notifications come back on)
		}
		m, _ := c.Store.Get(id)
		found = append(found, map[string]any{"plugin": id, "name": m.Name, "current": rec.Version, "new_version": latest,
			"reviewed": rec.Reviewed, "notes_url": notes})
		_ = c.Vault.Update(func(d *vault.Data) error {
			if p := d.Plugins[id]; p != nil {
				p.Notified = latest
			}
			return nil
		})
		log.Printf("[plugin %s] update available: %s -> %s", id, rec.Version, latest)
	}
	if len(found) == 0 {
		return
	}
	// One event for all of them: the Bot asks once, and the user approves them together.
	sort.Slice(found, func(i, j int) bool { return found[i]["plugin"].(string) < found[j]["plugin"].(string) })
	var ids []string
	for _, f := range found {
		ids = append(ids, `"`+f["plugin"].(string)+`"`)
	}
	data := map[string]any{"plugins": found,
		"next_step": "Tell the user; if they want them, call rubi_plugin_update ONCE with ids [" + strings.Join(ids, ", ") +
			"] (one approval for all) and send them the one approval link."}
	if len(found) == 1 { // the fields of a single update, as before, for Bots that read only those
		for k, v := range found[0] {
			data[k] = v
		}
	}
	c.Events.Emit("rubi", "plugin.update_available", data, nil)
}

// checkCatalogAge refuses a signed catalog older than one already seen: replaying an old one could bring
// back a pulled version or hide updates.
func (c *Core) checkCatalogAge(cat *plugins.Catalog) error {
	if c.State() != Unlocked || cat.UpdatedAt.IsZero() {
		return nil
	}
	var stale bool
	_ = c.Vault.Update(func(d *vault.Data) error {
		if cat.UpdatedAt.Before(d.CatalogSeen) {
			stale = true
			return errors.New("stale")
		}
		if cat.UpdatedAt.After(d.CatalogSeen) {
			d.CatalogSeen = cat.UpdatedAt.UTC()
		}
		return nil
	})
	if stale {
		c.Audit.Record("rubi.catalog_stale", audit.Fields{"updated_at": cat.UpdatedAt})
		return errors.New("the Rubi store returned an older list than before; try again later")
	}
	return nil
}
