package core

import (
	"context"
	"fmt"
	"slices"

	"github.com/Deikus-LXXVII/rubi/internal/update"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

// QuietUpdates reports whether the agent should not be told about new versions of id ("rubi" or a plugin).
func (c *Core) QuietUpdates(id string) bool {
	quiet := false
	_ = c.Vault.View(func(d *vault.Data) error {
		quiet = slices.Contains(d.QuietUpdates, id)
		return nil
	})
	return quiet
}

// SetUpdateNotify turns update notifications to the agent on or off for "rubi" or an installed plugin. It
// needs no approval: it only decides whether the agent is woken; updates themselves always need one.
func (c *Core) SetUpdateNotify(id string, notify bool) error {
	if id != "rubi" {
		if _, ok := c.Store.Get(id); !ok {
			return fmt.Errorf("%q is neither \"rubi\" nor an installed plugin", id)
		}
	}
	err := c.Vault.Update(func(d *vault.Data) error {
		d.QuietUpdates = slices.DeleteFunc(d.QuietUpdates, func(s string) bool { return s == id })
		if !notify {
			d.QuietUpdates = append(d.QuietUpdates, id)
		}
		return nil
	})
	if err == nil {
		c.Audit.Record("updates.notify", map[string]any{"id": id, "notify": notify})
		if notify && id == "rubi" {
			c.maybeNotifyUpdate()
		}
	}
	return err
}

// UpdateItem is one row of the Updates page.
type UpdateItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	Available bool   `json:"available"`
	Notify    bool   `json:"notify"`
	Reviewed  bool   `json:"reviewed,omitempty"`
	NotesURL  string `json:"notes_url,omitempty"`
}

// Updates lists Rubi and every installed plugin with their available updates and notification setting.
func (c *Core) Updates(ctx context.Context) []UpdateItem {
	info := c.UpdateInfo()
	items := []UpdateItem{{ID: "rubi", Name: "Rubi", Current: version.Version, Latest: info.Latest,
		Available: info.Available, Notify: !c.QuietUpdates("rubi"), Reviewed: true, NotesURL: info.NotesURL}}
	records := map[string]vault.Plugin{}
	_ = c.Vault.View(func(d *vault.Data) error {
		for id, p := range d.Plugins {
			records[id] = *p
		}
		return nil
	})
	cat, err := c.catalog(ctx, true) // what the user looks at is always current
	if err != nil {
		cat, _ = c.Catalog(ctx)
	}
	for _, m := range c.Store.Installed() {
		rec, ok := records[m.ID]
		if !ok {
			continue
		}
		it := UpdateItem{ID: m.ID, Name: m.Name, Current: rec.Version, Notify: !c.QuietUpdates(m.ID), Reviewed: rec.Reviewed}
		if rec.Reviewed {
			if e, ok := cat.Entry(m.ID); ok {
				if v, ok := e.Latest(version.Version); ok {
					it.Latest = v.Version
				}
			}
		} else if v, err := c.sideloadLatest(ctx, rec.Source); err == nil {
			it.Latest = v
		}
		it.Available = it.Latest != "" && update.Newer(it.Latest, rec.Version)
		items = append(items, it)
	}
	return items
}
