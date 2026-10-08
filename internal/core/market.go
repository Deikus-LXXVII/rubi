package core

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
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
				connected[id] = append(connected[id], map[string]any{"id": a.ID, "label": a.Label, "default": a == i.Find("")})
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
	if m, ok := c.Store.Get(id); ok && len(m.Config) > 0 {
		item["has_config"] = true
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

// RequestPluginInstall downloads and verifies a plugin, then asks the user to approve installing it.
// ref is a catalog id, or a source URL for sideloading.
func (c *Core) RequestPluginInstall(ctx context.Context, ref string) (map[string]any, error) {
	if c.State() != Unlocked {
		return nil, errors.New("Rubi is locked")
	}
	if !c.WebhookConfigured() {
		return nil, ErrNoWebhook
	}
	ref = strings.TrimSpace(ref)
	cat, catErr := c.Catalog(ctx)
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
		cand, err = c.fetchSideload(ctx, ref, "", cat)
		if err == nil && c.record(cand.Manifest.ID) != nil {
			os.RemoveAll(cand.Dir)
			return nil, fmt.Errorf("plugin %s is already installed; remove it first to install it from another source", cand.Manifest.ID)
		}
	}
	if err != nil {
		return nil, err
	}
	m := cand.Manifest
	preview := permissions(m, cand.Reviewed, cand.Source)
	return c.submitPluginChange(ctx, cand, "rubi.plugin.install", "Install "+m.Name+" "+m.Version, preview, func(option string) error {
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
		_ = c.SetUpdateNotify(m.ID, option != "install_quiet") // chosen on the install screen
		c.toolsChanged()
		c.startInstalled(m.ID, m.Version, cand.Tree)
		return nil
	}, func(res map[string]any) {
		if len(m.Fields)+len(m.Secrets) > 0 {
			res["next_step"] = "Installed. Now give the user the setup link: rubi_link(\"setup:" + m.ID + "\")."
		}
	})
}

// RequestPluginUpdate verifies the newest release of an installed plugin and asks to install it.
func (c *Core) RequestPluginUpdate(ctx context.Context, id string) (map[string]any, error) {
	if c.State() != Unlocked {
		return nil, errors.New("Rubi is locked")
	}
	rec := c.record(id)
	old, _ := c.Store.Get(id)
	if rec == nil {
		return nil, fmt.Errorf("plugin %q is not installed", id)
	}
	var cand *plugins.Candidate
	var err error
	if rec.Reviewed {
		cat, cerr := c.catalog(ctx, true)
		if cerr != nil {
			return nil, cerr
		}
		e, ok := cat.Entry(id)
		if !ok {
			return nil, fmt.Errorf("%s is no longer in the Rubi store", old.Name)
		}
		v, ok := e.Latest(version.Version)
		if !ok || !update.Newer(v.Version, rec.Version) {
			return map[string]any{"status": "up_to_date", "version": rec.Version}, nil
		}
		cand, err = c.fetchReviewed(ctx, e)
	} else {
		latest, lerr := c.sideloadLatest(ctx, rec.Source)
		if lerr != nil {
			return nil, lerr
		}
		if !update.Newer(latest, rec.Version) {
			return map[string]any{"status": "up_to_date", "version": rec.Version}, nil
		}
		cand, err = c.fetchSideload(ctx, rec.Source, rec.PublisherKey, nil)
	}
	if err != nil {
		return nil, err
	}
	m := cand.Manifest
	if m.ID != id {
		os.RemoveAll(cand.Dir)
		return nil, errors.New("the source now publishes a different plugin; refusing to update")
	}
	preview := permissions(m, cand.Reviewed, cand.Source)
	preview["current"] = rec.Version
	if added := newPermissions(old, m); added != "" {
		preview["new_permissions"] = added
	}
	return c.submitPluginChange(ctx, cand, "rubi.plugin.update", "Update "+m.Name+" to "+m.Version, preview, func(string) error {
		c.Runner.Stop(id)
		if err := c.Store.Commit(cand, rec.Version); err != nil { // keep the old version for rollback
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
		c.Audit.Record("plugin.updated", audit.Fields{"plugin": id, "from": rec.Version, "to": m.Version})
		c.toolsChanged()
		c.startInstalled(id, m.Version, cand.Tree)
		return nil
	}, nil)
}

// RequestPluginRollback asks to switch a plugin back to the version it replaced. Doing it again switches
// forward again. Settings, secrets and state stay.
func (c *Core) RequestPluginRollback(ctx context.Context, id string) (map[string]any, error) {
	if c.State() != Unlocked {
		return nil, errors.New("Rubi is locked")
	}
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
	preview := map[string]any{"plugin": m.Name, "current": rec.Version, "back_to": prev.Version,
		"effect": "Settings, passwords and state stay. You can switch forward again the same way."}
	return c.Approvals.Submit(ctx, approvals.Request{Integration: "rubi", Kind: "rubi.plugin.rollback",
		Summary: "Roll " + m.Name + " back to " + prev.Version, Preview: preview,
		Options: []approvals.Option{{Key: "rollback", Label: "Roll back"}},
		Execute: func(context.Context, string) (any, error) {
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
		}})
}

// RequestPluginRemove asks to uninstall a plugin and erase everything it stored.
func (c *Core) RequestPluginRemove(ctx context.Context, id string) (map[string]any, error) {
	if c.State() != Unlocked {
		return nil, errors.New("Rubi is locked")
	}
	m, ok := c.Store.Get(id)
	rec := c.record(id)
	if !ok && rec == nil {
		return nil, fmt.Errorf("plugin %q is not installed", id)
	}
	name := m.Name
	if name == "" {
		name = id
	}
	preview := map[string]any{"plugin": name, "effect": "Stops it and deletes it, with its stored passwords, settings and state."}
	return c.Approvals.Submit(ctx, approvals.Request{Integration: "rubi", Kind: "rubi.plugin.remove",
		Summary: "Remove " + name, Preview: preview, Options: []approvals.Option{{Key: "remove", Label: "Remove"}},
		Execute: func(context.Context, string) (any, error) {
			c.Runner.Stop(id)
			if err := c.Vault.Update(func(d *vault.Data) error {
				delete(d.Plugins, id)
				delete(d.Integrations, id)
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
		}})
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
		// The user picks, and signs, whether the agent hears about this plugin's updates.
		question = "Tell your agent when new versions come out?"
		options = []approvals.Option{{Key: "install_quiet", Label: "Install", Meaning: "updates wait on the Updates page"},
			{Key: "install", Label: "Install and notify about updates", Meaning: "your agent tells you about new versions"}}
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

func (c *Core) unstage(dir string) {
	c.mkt.mu.Lock()
	delete(c.mkt.staged, dir)
	c.mkt.mu.Unlock()
	_ = os.RemoveAll(dir)
}

func (c *Core) cleanupWhenDone(approvalID, dir string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
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
	c.cleanStaging()
	cand, err := plugins.Download(ctx, base, c.Store.Staging(), plugins.Expect{ID: e.ID, PublisherKey: e.Publisher.Key,
		SumsSHA256: v.SumsSHA256})
	if err != nil {
		return nil, err
	}
	if cand.Manifest.Version != v.Version {
		os.RemoveAll(cand.Dir)
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
	c.cleanStaging()
	cand, err := plugins.Download(ctx, base, c.Store.Staging(), plugins.Expect{ID: m.ID, PublisherKey: key})
	if err != nil {
		return nil, err
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
	rubiplugin.Strong: "Passkey / password"}

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
	if old.Publisher.Key != "" && old.Publisher.Key != m.Publisher.Key {
		added = append(added, "new publisher key "+plugins.Fingerprint(m.Publisher.Key))
	}
	return strings.Join(added, "; ")
}

func levelRank(l rubiplugin.Level) int {
	return map[rubiplugin.Level]int{rubiplugin.None: 0, rubiplugin.Chat: 1, rubiplugin.Strong: 2}[l]
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
		c.Events.Emit("rubi", "plugin.update_available", map[string]any{"plugin": id, "name": m.Name,
			"current": rec.Version, "new_version": latest, "reviewed": rec.Reviewed, "notes_url": notes,
			"next_step": "Tell the user; if they want it, call rubi_plugin_update(\"" + id + "\") and send them the approval link."}, nil)
		_ = c.Vault.Update(func(d *vault.Data) error {
			if p := d.Plugins[id]; p != nil {
				p.Notified = latest
			}
			return nil
		})
		log.Printf("[plugin %s] update available: %s -> %s", id, rec.Version, latest)
	}
}
