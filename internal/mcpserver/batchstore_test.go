package mcpserver_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/plugins/plugintest"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// Project rule 5 (docs/project-rules.md), the store: several plugins to install, remove or roll back are one
// approval with an item each, and only the ticked items happen.

// storeRig has two reviewed plugins (alpha, beta) in the store and a third, gamma, that can only be sideloaded.
func storeRig(t *testing.T) (*rig, *plugintest.Registry) {
	t.Helper()
	reg := plugintest.New(t)
	for _, id := range []string{"alpha", "beta"} {
		reg.ReviewAs(id, "v1.0.0", reg.PublishAs(id, "v1.0.0", nil, nil), "")
	}
	reg.PublishAs("gamma", "v1.0.0", nil, nil)
	return newRig(t), reg
}

// waitingKind counts the approvals of a kind that wait for the user.
func waitingKind(r *rig, kind string) int {
	n := 0
	for _, a := range r.c.Approvals.Pending() {
		if a.Kind == kind {
			n++
		}
	}
	return n
}

func installedPlugins(r *rig) []string {
	var ids []string
	_ = r.c.Vault.View(func(d *vault.Data) error {
		for id := range d.Plugins {
			ids = append(ids, id)
		}
		return nil
	})
	slices.Sort(ids)
	return ids
}

func versionOf(r *rig, id string) string {
	v := ""
	_ = r.c.Vault.View(func(d *vault.Data) error {
		if p := d.Plugins[id]; p != nil {
			v = p.Version
		}
		return nil
	})
	return v
}

// approveItems approves a subset of a batch with the password and returns the approval's result.
func approveItems(r *rig, out map[string]any, option string) map[string]any {
	r.t.Helper()
	id := out["approval_id"].(string)
	res, err := r.panelLink(out["approval_url"].(string)).ApproveWithPassword(id, option, pw)
	if err != nil || res["state"] != "executed" {
		r.t.Fatalf("approve %s %s: %v %v", id, option, res, err)
	}
	snap := r.ag.call("rubi_approval", map[string]any{"approval_id": id})["approval"].(map[string]any)
	result, _ := snap["result"].(map[string]any)
	return result
}

func itemPreviews(t *testing.T, r *rig, out map[string]any) map[string]map[string]any {
	t.Helper()
	info, err := r.panelLink(out["approval_url"].(string)).ApprovalInfo(out["approval_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for _, it := range info.Approval.Items {
		var p map[string]any
		if err := json.Unmarshal(it.Preview, &p); err != nil {
			t.Fatalf("item %s preview: %v", it.Key, err)
		}
		p["_label"] = it.Label
		got[it.Key] = p
	}
	return got
}

func TestSeveralPluginsInstallWithOneApproval(t *testing.T) {
	r, reg := storeRig(t)
	out := r.ag.call("rubi_plugin_install", map[string]any{"plugins": []string{"alpha", "beta", reg.SourceOf("gamma")}})
	if out["approval_id"] == nil || out["tool_error"] != nil {
		t.Fatalf("install of three: %v", out)
	}
	if n := waitingKind(r, "rubi.plugin.install"); n != 1 {
		t.Fatalf("%d install approvals wait; want 1 for three plugins", n)
	}
	items := itemPreviews(t, r, out)
	if len(items) != 3 {
		t.Fatalf("items: %v", items)
	}
	// Each item carries what the single approval shows: publisher key, review, version, capabilities.
	for _, id := range []string{"alpha", "beta"} {
		p := items[id]
		if p["review"] != "Reviewed by Rubi-Project" || p["warning"] != nil || p["new_version"] != "v1.0.0" ||
			!strings.Contains(p["publisher"].(string), " · key ") || !strings.Contains(p["can"].(string), "Send (Passkey / password)") ||
			p["connects_to"] != "example.com:443" || p["will_ask_for"] != "User, Token" {
			t.Fatalf("%s preview: %v", id, p)
		}
	}
	// An unofficially sourced plugin stands out in its own item: warning, no review, its label says so.
	g := items["gamma"]
	if g["review"] != "Not reviewed by Rubi-Project" || g["warning"] == nil || g["source"] != reg.SourceOf("gamma") ||
		!strings.Contains(g["_label"].(string), "not reviewed") || strings.Contains(items["alpha"]["_label"].(string), "not reviewed") {
		t.Fatalf("gamma preview: %v", g)
	}
	info, _ := r.panelLink(out["approval_url"].(string)).ApprovalInfo(out["approval_id"].(string))
	if top := previewOf(t, info); top["warning"] == nil {
		t.Fatalf("the batch does not warn about the unreviewed plugin: %v", top)
	}
	if res := r.ag.call("rubi_confirm", map[string]any{"approval_id": out["approval_id"], "user_response": "Install"}); res["tool_error"] == nil {
		t.Fatal("the agent approved an install itself")
	}
	// A subset: gamma is unticked and alpha and beta are installed.
	res := approveItems(r, out, "install_quiet|items:alpha,beta")
	if got := toString(res["installed"]); !strings.Contains(got, "alpha") || !strings.Contains(got, "beta") || strings.Contains(got, "gamma") {
		t.Fatalf("result: %v", res)
	}
	if got := installedPlugins(r); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("installed after a subset: %v", got)
	}
	if n := waitingKind(r, "rubi.plugin.install"); n != 0 {
		t.Fatalf("%d install approvals still wait", n)
	}
}

func TestInstallOnlyInstallsTheTickedItems(t *testing.T) {
	r, reg := storeRig(t)
	out := r.ag.call("rubi_plugin_install", map[string]any{"plugins": []string{"alpha", "beta", reg.SourceOf("gamma")}})
	approveItems(r, out, "install|items:gamma")
	if got := installedPlugins(r); !slices.Equal(got, []string{"gamma"}) {
		t.Fatalf("installed: %v; want only the ticked gamma", got)
	}
	if _, err := os.Stat(filepath.Join(r.c.Layout.Plugins(), "alpha")); !os.IsNotExist(err) {
		t.Fatalf("alpha's files exist though it was not ticked: %v", err)
	}
}

func TestRefusedBatchInstallsNothing(t *testing.T) {
	r, _ := storeRig(t)
	out := r.ag.call("rubi_plugin_install", map[string]any{"plugins": []string{"alpha", "beta"}})
	if _, err := r.panelLink(out["approval_url"].(string)).Deny(out["approval_id"].(string)); err != nil {
		t.Fatal(err)
	}
	if got := installedPlugins(r); len(got) != 0 {
		t.Fatalf("installed after a refusal: %v", got)
	}
	// Nothing in the batch can be approved after the refusal.
	if _, err := r.panelLink(out["approval_url"].(string)).ApproveWithPassword(out["approval_id"].(string), "install_quiet|items:alpha", pw); err == nil {
		t.Fatal("a refused batch was approved")
	}
	if got := installedPlugins(r); len(got) != 0 {
		t.Fatalf("installed after a refusal: %v", got)
	}
}

func TestOneFailingPluginDoesNotHideTheOthers(t *testing.T) {
	r, _ := storeRig(t)
	out := r.ag.call("rubi_plugin_install", map[string]any{"plugins": []string{"alpha", "beta", "nope"}})
	// "nope" is not in the store: reported at once, the rest is still one approval.
	if failed, _ := out["failed"].(map[string]any); failed["nope"] == nil || out["approval_url"] == nil {
		t.Fatalf("a ref that can't be fetched: %v", out)
	}
	// beta's files are changed on disk before the user approves: its install fails, alpha's does not.
	staged, _ := filepath.Glob(filepath.Join(r.c.Store.Staging(), ".staging-beta-*"))
	if len(staged) != 1 {
		t.Fatalf("staging of beta: %v", staged)
	}
	if err := os.WriteFile(filepath.Join(staged[0], "extra"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := approveItems(r, out, "install_quiet|items:alpha,beta")
	failed, _ := res["failed"].(map[string]any)
	if failed["beta"] == nil || failed["alpha"] != nil || !strings.Contains(toString(res["installed"]), "alpha") {
		t.Fatalf("per-item report: %v", res)
	}
	if got := installedPlugins(r); !slices.Equal(got, []string{"alpha"}) {
		t.Fatalf("installed: %v", got)
	}
}

func TestSinglePluginInstallStaysTheOrdinaryApproval(t *testing.T) {
	r, _ := storeRig(t)
	for _, args := range []map[string]any{{"plugin": "alpha"}, {"plugins": []string{"alpha"}}} {
		out := r.ag.call("rubi_plugin_install", args)
		info, err := r.panelLink(out["approval_url"].(string)).ApprovalInfo(out["approval_id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if len(info.Approval.Items) != 0 || previewOf(t, info)["review"] != "Reviewed by Rubi-Project" || info.Approval.Summary != "Install Demo alpha v1.0.0" {
			t.Fatalf("%v: %+v", args, info.Approval)
		}
		if _, err := r.panelLink(out["approval_url"].(string)).Deny(out["approval_id"].(string)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSeveralPluginsRemoveWithOneApproval(t *testing.T) {
	r, reg := storeRig(t)
	out := r.ag.call("rubi_plugin_install", map[string]any{"plugins": []string{"alpha", "beta", reg.SourceOf("gamma")}})
	approveItems(r, out, "install_quiet|items:alpha,beta,gamma") // gamma is not from the store: ticked on purpose
	if got := installedPlugins(r); len(got) != 3 {
		t.Fatalf("installed: %v", got)
	}
	out = r.ag.call("rubi_plugin_remove", map[string]any{"ids": []string{"alpha", "beta", "gamma", "nope"}})
	if failed, _ := out["failed"].(map[string]any); failed["nope"] == nil || out["approval_url"] == nil {
		t.Fatalf("remove: %v", out)
	}
	if n := waitingKind(r, "rubi.plugin.remove"); n != 1 {
		t.Fatalf("%d remove approvals wait; want 1", n)
	}
	if items := itemPreviews(t, r, out); len(items) != 3 || items["alpha"]["effect"] == nil || items["alpha"]["plugin"] != "Demo alpha" {
		t.Fatalf("items: %v", items)
	}
	res := approveItems(r, out, "items:alpha,gamma") // beta stays
	if got := toString(res["removed"]); !strings.Contains(got, "alpha") || !strings.Contains(got, "gamma") || strings.Contains(got, "beta") {
		t.Fatalf("result: %v", res)
	}
	if got := installedPlugins(r); !slices.Equal(got, []string{"beta"}) {
		t.Fatalf("installed after a subset removal: %v", got)
	}
	if _, err := os.Stat(filepath.Join(r.c.Layout.Plugins(), "alpha")); !os.IsNotExist(err) {
		t.Fatalf("alpha's files left: %v", err)
	}
	// A single id is the ordinary approval and its result keeps its shape.
	out = r.ag.call("rubi_plugin_remove", map[string]any{"id": "beta"})
	if res := r.approve(out, "remove"); res["removed"] != "beta" {
		t.Fatalf("single remove: %v", res)
	}
}

func TestSeveralPluginsRollBackWithOneApproval(t *testing.T) {
	r, reg := storeRig(t)
	out := r.ag.call("rubi_plugin_install", map[string]any{"plugins": []string{"alpha", "beta"}})
	approveItems(r, out, "install_quiet")
	for _, id := range []string{"alpha", "beta"} { // both get a v1.1.0 and are updated together
		reg.ReviewAs(id, "v1.1.0", reg.PublishAs(id, "v1.1.0", nil, nil), "")
	}
	out = r.ag.call("rubi_plugin_update", map[string]any{"all": true})
	if res := approveItems(r, out, "items:alpha,beta"); len(res["updated"].([]any)) != 2 {
		t.Fatalf("update: %v", res)
	}
	if versionOf(r, "alpha") != "v1.1.0" || versionOf(r, "beta") != "v1.1.0" {
		t.Fatalf("after the update: %s %s", versionOf(r, "alpha"), versionOf(r, "beta"))
	}

	out = r.ag.call("rubi_plugin_rollback", map[string]any{"ids": []string{"alpha", "beta"}})
	if n := waitingKind(r, "rubi.plugin.rollback"); n != 1 {
		t.Fatalf("%d rollback approvals wait; want 1", n)
	}
	items := itemPreviews(t, r, out)
	if len(items) != 2 || items["alpha"]["current"] != "v1.1.0" || items["alpha"]["back_to"] != "v1.0.0" {
		t.Fatalf("items: %v", items)
	}
	res := approveItems(r, out, "items:beta") // only beta goes back
	if !strings.Contains(toString(res["rolled_back"]), "beta") || strings.Contains(toString(res["rolled_back"]), "alpha") {
		t.Fatalf("result: %v", res)
	}
	if versionOf(r, "alpha") != "v1.1.0" || versionOf(r, "beta") != "v1.0.0" {
		t.Fatalf("after a subset rollback: alpha %s beta %s", versionOf(r, "alpha"), versionOf(r, "beta"))
	}
	// The one that can't go back (alpha now has a previous; a plugin that was never updated has none) is
	// reported without holding back the rest.
	out = r.ag.call("rubi_plugin_rollback", map[string]any{"ids": []string{"alpha", "nope"}})
	if failed, _ := out["failed"].(map[string]any); failed["nope"] == nil || out["approval_url"] == nil {
		t.Fatalf("rollback with an unknown id: %v", out)
	}
	if _, err := r.panelLink(out["approval_url"].(string)).Deny(out["approval_id"].(string)); err != nil {
		t.Fatal(err)
	}
}

// The engine's own rule behind "only the ticked items": an item that isn't in the batch can't be chosen.
func TestBatchOptionRejectsUnknownItems(t *testing.T) {
	r, _ := storeRig(t)
	out := r.ag.call("rubi_plugin_install", map[string]any{"plugins": []string{"alpha", "beta"}})
	id := out["approval_id"].(string)
	if err := r.c.Approvals.CheckOption(id, "install|items:alpha,gamma"); err == nil {
		t.Fatal("an item that is not in the request was accepted")
	}
	if err := r.c.Approvals.CheckOption(id, "install|items:"); err == nil {
		t.Fatal("an empty selection was accepted")
	}
}

// The panel asks the same way: ops with a list give one approval with an item each.
func TestPanelAsksForSeveralPluginsAtOnce(t *testing.T) {
	r, _ := storeRig(t)
	settings := r.panel("settings")
	var res map[string]any
	if err := settings.Call("plugin.install", map[string]any{"plugins": []string{"alpha", "beta"}}, &res); err != nil {
		t.Fatal(err)
	}
	pc := *settings
	pc.Link.Ticket = res["ticket"].(string)
	info, err := pc.ApprovalInfo(res["approval_id"].(string))
	if err != nil || len(info.Approval.Items) != 2 || info.Approval.Kind != "rubi.plugin.install" {
		t.Fatalf("panel install of two: %+v %v", info, err)
	}
	if out, err := pc.ApproveWithPassword(res["approval_id"].(string), "install_quiet|items:beta", pw); err != nil || out["state"] != "executed" {
		t.Fatalf("approve: %v %v", out, err)
	}
	if got := installedPlugins(r); !slices.Equal(got, []string{"beta"}) {
		t.Fatalf("installed: %v", got)
	}
	// An older panel's single id still works for remove.
	res = nil
	if err := settings.Call("plugin.remove", map[string]any{"id": "beta"}, &res); err != nil || res["approval_id"] == nil {
		t.Fatalf("panel remove of one: %v %v", res, err)
	}
	res = nil
	if err := settings.Call("plugin.remove", map[string]any{"ids": []string{"beta", "nope"}}, &res); err != nil || res["approval_id"] == nil {
		t.Fatalf("panel remove with an unknown one: %v %v", res, err)
	}
}

// A plugin from outside the store starts unticked in a batch: approving the request as it stands installs the
// reviewed ones only, and the other needs a tick of its own.
func TestUnreviewedPluginInABatchStartsUnticked(t *testing.T) {
	r, reg := storeRig(t)
	out := r.ag.call("rubi_plugin_install", map[string]any{"plugins": []string{"alpha", reg.SourceOf("gamma"), "beta"}})
	info, err := r.panelLink(out["approval_url"].(string)).ApprovalInfo(out["approval_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	off := map[string]bool{}
	for _, it := range info.Approval.Items {
		off[it.Key] = it.DefaultOff
	}
	if len(off) != 3 || off["alpha"] || off["beta"] || !off["gamma"] {
		t.Fatalf("default_off per item: %v", off)
	}
	// the panel's plain "approve" (the option key) carries the reviewed ones only
	res := approveItems(r, out, "install_quiet")
	if got := toString(res["installed"]); !strings.Contains(got, "alpha") || !strings.Contains(got, "beta") || strings.Contains(got, "gamma") {
		t.Fatalf("result: %v", res)
	}
	if got := installedPlugins(r); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("installed: %v", got)
	}
}

func TestTickingTheUnreviewedItemInstallsIt(t *testing.T) {
	r, reg := storeRig(t)
	out := r.ag.call("rubi_plugin_install", map[string]any{"plugins": []string{"alpha", reg.SourceOf("gamma")}})
	approveItems(r, out, "install_quiet|items:alpha,gamma")
	if got := installedPlugins(r); !slices.Equal(got, []string{"alpha", "gamma"}) {
		t.Fatalf("installed after ticking gamma: %v", got)
	}
}

// A single unreviewed plugin stays its own approval with its warning, as before.
func TestSingleUnreviewedInstallKeepsItsWarning(t *testing.T) {
	r, reg := storeRig(t)
	out := r.ag.call("rubi_plugin_install", map[string]any{"plugins": []string{reg.SourceOf("gamma")}})
	info, err := r.panelLink(out["approval_url"].(string)).ApprovalInfo(out["approval_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	p := previewOf(t, info)
	if len(info.Approval.Items) != 0 || p["warning"] == nil || p["review"] != "Not reviewed by Rubi-Project" {
		t.Fatalf("%+v %v", info.Approval, p)
	}
	approveItems(r, out, "install_quiet")
	if got := installedPlugins(r); !slices.Equal(got, []string{"gamma"}) {
		t.Fatalf("installed: %v", got)
	}
}
