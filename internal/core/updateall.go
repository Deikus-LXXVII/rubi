package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// updateAllKind is the approval that updates Rubi and plugins together (project rule 5: fewest user actions).
const updateAllKind = "rubi.update.all"

// Item keys of that approval: Rubi's own update, and "plugin:<id>" for each plugin.
const (
	updateAllRubi         = "rubi"
	updateAllPluginPrefix = "plugin:"
)

// pendingAll is the "update everything" approval that waits, so asking again gives the same link.
type pendingAll struct {
	approvalID string
	key        string // what it covers: the Rubi version and each plugin with its version
	res        map[string]any
}

// RequestUpdateAll asks to update Rubi and every plugin that has an update with ONE approval: Rubi is one item,
// each plugin another, and the user can untick any. With only Rubi or only plugins to update it is the
// ordinary approval of those.
//
// The order when approved is plugins first, then Rubi. Rubi's update ends with the restart (the unlocked
// handoff, see armRestart), and the restart ends this process: the plugin updates have been verified and staged
// in it, so they are applied before the restart is armed, and one failing Rubi download cannot undo or hold back
// them. The restart is armed once, by Rubi's own step, and not at all when its item is unticked.
func (c *Core) RequestUpdateAll(ctx context.Context) (map[string]any, error) {
	if c.State() != Unlocked {
		return nil, errors.New("Rubi is locked; unlock it first")
	}
	info, available, rerr := c.prepareRubiUpdate(ctx)
	if !available {
		// Nothing for Rubi (or it can't be checked): the plugins' update, and a word about Rubi.
		res, err := c.RequestPluginUpdates(ctx, nil)
		switch {
		case rerr != nil && err != nil:
			return nil, err
		case rerr != nil:
			failed, _ := res["failed"].(map[string]string)
			if failed == nil {
				failed = map[string]string{}
			}
			failed["rubi"] = rerr.Error()
			res["failed"] = failed
		case res != nil:
			res["rubi"] = "up_to_date"
		}
		return res, err
	}

	// Plugins with an update; the ones the user switched notifications off for are part of "everything".
	_, _ = c.catalog(ctx, true) // current, not the copy kept for a few minutes (updateAvailable reads that)
	var ids []string
	_ = c.Vault.View(func(d *vault.Data) error {
		for id := range d.Plugins {
			ids = append(ids, id)
		}
		return nil
	})
	sort.Strings(ids)
	var withUpdate []string
	leftOut := 0 // plugins with an update beyond what one approval holds
	for _, id := range ids {
		if ok, err := c.updateAvailable(ctx, id); ok && err == nil {
			if len(withUpdate) < maxBatch {
				withUpdate = append(withUpdate, id)
			} else {
				leftOut++
			}
		}
	}
	if len(withUpdate) == 0 {
		return c.submitRubiUpdate(ctx, info)
	}
	ready, out := c.prepareUpdates(ctx, withUpdate)
	if len(ready) == 0 {
		res, err := c.submitRubiUpdate(ctx, info)
		if err == nil {
			for k, v := range out {
				res[k] = v
			}
		}
		return res, err
	}
	return c.submitUpdateAll(ctx, info, ready, out, leftOut)
}

func (c *Core) submitUpdateAll(ctx context.Context, info UpdateInfo, ready []*pluginUpdate, out map[string]any, leftOut int) (map[string]any, error) {
	target := info.Latest
	items := []approvals.Item{{Key: updateAllRubi, Label: "Rubi " + info.Current + " → " + target, Preview: rubiUpdatePreview(info)}}
	byID := map[string]*pluginUpdate{}
	key := target + "|left_out=" + fmt.Sprint(leftOut)
	for _, u := range ready {
		item := approvals.Item{Key: updateAllPluginPrefix + u.id, Label: u.name + " " + u.from + " → " + u.cand.Manifest.Version, Preview: u.preview}
		if off, why := u.startsUnticked(); off {
			// Not ticked for the user: an update from outside the store that changes who signs it or what it may do.
			p := map[string]any{}
			for k, v := range u.preview {
				p[k] = v
			}
			p["starts_unticked"] = "Not ticked: " + why + ". Tick it only if you trust this update."
			item.Preview, item.DefaultOff = p, true
			item.Label += " (not ticked: " + strings.SplitN(why, ":", 2)[0] + ")"
		}
		items = append(items, item)
		byID[u.id] = u
		key += "|" + u.id + "=" + u.cand.Manifest.Version
	}

	c.mkt.updMu.Lock()
	defer c.mkt.updMu.Unlock()
	if p := c.mkt.pendingAll; p != nil {
		if snap, err := c.Approvals.Get(p.approvalID); err != nil || snap.State != approvals.Pending || time.Until(snap.ExpiresAt) < 2*time.Minute {
			c.mkt.pendingAll = nil
		} else if p.key == key {
			for _, u := range ready {
				c.unstage(u.cand.Dir)
			}
			reply := map[string]any{}
			for k, v := range p.res {
				reply[k] = v
			}
			reply["note"] = "This update already waits for the user's approval at this link; send it if you haven't."
			return reply, nil
		}
	}

	unstageAll := func() {
		for _, u := range ready {
			c.unstage(u.cand.Dir)
		}
	}
	preview := map[string]any{"plugins": len(ready),
		"note": "Everything is verified against its signed release. The plugins are updated first, then Rubi restarts and stays unlocked. Uncheck any you'd rather keep as it is."}
	if leftOut > 0 {
		preview["left_out"] = fmt.Sprintf("%s with an update did not fit in this approval (at most %d at a time) and %s left out. Update %s afterwards.",
			plural(leftOut, "more plugin"), maxBatch, map[bool]string{true: "is", false: "are"}[leftOut == 1], map[bool]string{true: "it", false: "them"}[leftOut == 1])
	}
	res, err := c.Approvals.Submit(ctx, approvals.Request{Integration: "rubi", Kind: updateAllKind,
		Summary: fmt.Sprintf("Update Rubi and %s", plural(len(ready), "plugin")),
		Preview: preview,
		Options: []approvals.Option{{Key: "update", Label: "Update"}}, Items: items,
		Execute: func(ctx context.Context, option string) (any, error) {
			defer unstageAll()
			_, chosen := approvals.SplitOption(option)
			var plugins []string
			rubi := false
			for _, k := range chosen {
				if k == updateAllRubi {
					rubi = true
				} else if id, ok := strings.CutPrefix(k, updateAllPluginPrefix); ok {
					plugins = append(plugins, id)
				}
			}
			updated, errs := runPluginUpdates(plugins, byID) // plugins first
			result := map[string]any{"updated": updated}
			if len(errs) > 0 {
				result["failed"] = errs
			}
			done := len(updated) > 0
			if rubi { // then Rubi, whose step arms the one restart
				r, err := c.applyUpdate(ctx, target)
				if err != nil {
					result["rubi_failed"] = err.Error()
				} else {
					result["rubi"], result["restarting"], done = r, true, true
				}
			}
			if leftOut > 0 {
				result["left_out"] = leftOut // more plugins have updates: ask again for them
			}
			if !done {
				return result, errors.New("nothing could be updated")
			}
			return result, nil
		}})
	if err != nil {
		unstageAll()
		return nil, err
	}
	got := make([]string, len(ready))
	for i, u := range ready {
		got[i] = u.id
	}
	res["plugins"] = got
	if leftOut > 0 {
		res["left_out"] = leftOut
		res["note"] = fmt.Sprintf("%s did not fit in this approval; once the user has decided it, ask again for them.", plural(leftOut, "more plugin"))
	}
	id, _ := res["approval_id"].(string)
	if id != "" {
		go func() {
			for _, u := range ready {
				c.cleanupWhenDone(id, u.cand.Dir)
			}
		}()
		// This approval covers the plugins of a plugin-update approval that waits: that one is not needed.
		if p := c.mkt.pendingUpdate; p != nil {
			_, _ = c.Approvals.Cancel(p.approvalID)
			c.mkt.pendingUpdate = nil
		}
		if old := c.mkt.pendingAll; old != nil {
			_, _ = c.Approvals.Cancel(old.approvalID)
		}
		p := &pendingAll{approvalID: id, key: key, res: map[string]any{}}
		for k, v := range res {
			p.res[k] = v
		}
		c.mkt.pendingAll = p
	}
	for k, v := range out {
		res[k] = v
	}
	return res, nil
}

// cancelPendingAll withdraws the waiting "update everything" approval (c.mkt.updMu is held). Plugin updates and
// the combined approval share one slot: whichever is asked for last is the one that waits, and the earlier one can
// no longer be approved, so two approvals never wait to install different releases of the same plugin.
func (c *Core) cancelPendingAll() {
	if p := c.mkt.pendingAll; p != nil {
		_, _ = c.Approvals.Cancel(p.approvalID)
		c.mkt.pendingAll = nil
	}
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// restartsForUpdate is an update approval that has armed the restart (its result says so); the new process
// reports update.completed, so the agent is not also told of this approval.
func restartsForUpdate(s approvals.Snapshot) bool {
	if s.State != approvals.Executed {
		return false
	}
	switch s.Kind {
	case "rubi.update":
		return true
	case updateAllKind:
		m, _ := s.Result.(map[string]any)
		return m["restarting"] == true
	}
	return false
}
