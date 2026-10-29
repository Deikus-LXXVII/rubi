package core

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/update"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

// StoreQuery narrows rubi_store. Empty: the whole list, as before.
type StoreQuery struct {
	Query      string // words that must all appear in the id, name, summary or declared capabilities
	Capability string // a capability: an action kind or title, a tool, an event type, a Rubi Home operation
}

func (q StoreQuery) empty() bool {
	return strings.TrimSpace(q.Query) == "" && strings.TrimSpace(q.Capability) == ""
}

// StoreSearch finds plugins in the signed store catalog. Only reviewed plugins of the verified catalog are
// searched (never sideloaded ones). It is read-only.
func (c *Core) StoreSearch(ctx context.Context, q StoreQuery) (map[string]any, error) {
	if q.empty() {
		return c.StoreList(ctx)
	}
	if len(q.Query) > 200 || len(q.Capability) > 100 {
		return nil, errors.New("the search is too long")
	}
	cat, catErr := c.catalog(ctx, true)
	if catErr != nil {
		cat, _ = c.Catalog(ctx) // the last verified copy
	}
	if cat == nil {
		return nil, errors.New("the store catalog can't be read right now: " + catErr.Error())
	}
	records := map[string]vault.Plugin{}
	connected := map[string][]map[string]any{}
	_ = c.Vault.View(func(d *vault.Data) error {
		for id, p := range d.Plugins {
			records[id] = *p
		}
		for id, i := range d.Integrations {
			for _, a := range i.Accounts {
				connected[id] = append(connected[id], map[string]any{"id": a.ID, "label": a.Label, "default": a == i.Find(""), "agents": a.Agents})
			}
		}
		return nil
	})
	installed := func(id string) (plugins.Manifest, bool) {
		if _, ok := records[id]; !ok {
			return plugins.Manifest{}, false
		}
		return c.Store.Get(id)
	}
	found := searchCatalog(cat, installed, q, version.Version)
	for _, item := range found {
		id := item["id"].(string)
		c.describeInstalled(item, id, records, connected)
		if rec, ok := records[id]; ok {
			if l, ok := item["latest"].(string); ok && update.Newer(l, rec.Version) {
				item["update_available"] = l
			}
		}
		switch {
		case item["installed"] != true:
			item["next_step"] = "rubi_plugin_install {plugin: \"" + id + "\"}: the user sees its permissions in the install approval"
		case item["connected"] != true:
			item["next_step"] = "send the user rubi_link(\"setup:" + id + "\") to connect an account"
		default:
			item["next_step"] = "to use its accounts as a Bot: rubi_access_request {agent, plugins: [{id: \"" + id + "\"}]}"
		}
	}
	out := map[string]any{"plugins": found, "query": q.Query, "capability": q.Capability, "matches": len(found)}
	if catErr != nil {
		out["catalog_error"] = catErr.Error()
	}
	return out, nil
}

// searchCatalog matches catalog entries. What a plugin needs (its actions, tools, events, network and
// Rubi Home access) is known from its signed manifest once installed; for the others it is shown in the
// install approval, and a capability search can only match their store text.
func searchCatalog(cat *plugins.Catalog, installed func(string) (plugins.Manifest, bool), q StoreQuery, rubi string) []map[string]any {
	words := strings.Fields(strings.ToLower(q.Query))
	capq := strings.ToLower(strings.TrimSpace(q.Capability))
	out := []map[string]any{}
	for i := range cat.Plugins {
		e := &cat.Plugins[i]
		m, have := installed(e.ID)
		caps := []string{}
		if have {
			caps = capabilities(m)
		}
		text := strings.ToLower(strings.Join([]string{e.ID, e.Name, e.Summary, m.Description}, " "))
		capText := strings.ToLower(strings.Join(caps, " "))
		ok := true
		for _, w := range words {
			ok = ok && (strings.Contains(text, w) || strings.Contains(capText, w))
		}
		matchedBy := ""
		if ok && capq != "" {
			switch {
			case have && strings.Contains(capText, capq):
				matchedBy = "declared capabilities"
			case !have && strings.Contains(text, capq):
				matchedBy = "store description only (its declared capabilities are known after download)"
			default:
				ok = false
			}
		}
		if !ok {
			continue
		}
		item := map[string]any{"id": e.ID, "name": e.Name, "summary": e.Summary, "publisher": e.Publisher.Name,
			"reviewed": true, "source": e.Source}
		if v, ok := e.Latest(rubi); ok {
			item["latest"], item["min_rubi"] = v.Version, v.MinRubi
		} else {
			item["requires_newer_rubi"] = true
			var newest plugins.CatalogVersion
			for _, v := range e.Versions {
				if update.ValidVersion(v.Version) && (newest.Version == "" || update.Newer(v.Version, newest.Version)) {
					newest = v
				}
			}
			item["newest"], item["min_rubi"] = newest.Version, newest.MinRubi
		}
		if matchedBy != "" {
			item["capability_matched_by"] = matchedBy
		}
		if have {
			item["needs"] = needsOf(m)
		} else {
			item["needs"] = "shown to the user in the install approval, from the signed package"
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	return out
}

// capabilities are what a plugin declares it can do, as searchable words.
func capabilities(m plugins.Manifest) []string {
	var out []string
	for _, a := range m.Actions {
		out = append(out, a.Kind, a.Title)
	}
	for _, t := range m.Tools {
		out = append(out, t.Name)
	}
	for _, e := range m.Events {
		out = append(out, e.Type)
	}
	out = append(out, m.Home...)
	if m.Hooks {
		out = append(out, "hooks")
	}
	return out
}

// needsOf is what an installed plugin may do and asks for, as its signed manifest declares.
func needsOf(m plugins.Manifest) map[string]any {
	var acts []map[string]any
	for _, a := range m.Actions {
		acts = append(acts, map[string]any{"kind": a.Kind, "title": a.Title, "default_level": a.DefaultLevel})
	}
	var tools, evs, secrets []string
	for _, t := range m.Tools {
		tools = append(tools, t.Name)
	}
	for _, e := range m.Events {
		evs = append(evs, e.Type)
	}
	for _, s := range m.Secrets {
		if !s.Internal {
			secrets = append(secrets, s.Label)
		}
	}
	n := map[string]any{"actions": acts, "tools": tools, "events": evs, "connects_to": m.Egress}
	if len(secrets) > 0 {
		n["asks_user_for"] = secrets
	}
	if len(m.Home) > 0 {
		n["rubi_home"] = m.Home
	}
	if m.Hooks {
		n["hooks"] = true
	}
	return n
}
