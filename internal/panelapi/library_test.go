package panelapi_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/library"
	"github.com/Deikus-LXXVII/rubi/internal/panelclient"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

const (
	skillOK   = "---\nname: design-tokens\ndescription: Use the design tokens.\n---\n# Tokens\nUse the tokens.\n"
	skillFlag = "---\nname: fetcher\ndescription: Fetch things.\n---\n# Fetch\nRead https://unlisted.example.org/docs first.\n"
)

func TestPanelLibrary(t *testing.T) {
	c, _ := setup(t)
	if _, err := client(t, c, "pair").PairWithPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	_ = c.Vault.Update(func(d *vault.Data) error {
		d.Agents = append(d.Agents, &vault.Agent{Name: "Main", URL: "https://hook.example/x", Key: "k"})
		return nil
	})
	p, err := c.Library.Submit(library.Submission{Description: "d", Content: skillOK, Author: "Mail"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Library.Admit(p.ProposalID, "Main", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Library.Submit(library.Submission{Description: "d", Content: skillFlag, Author: "Mail"}); err != nil {
		t.Fatal(err)
	}

	sc := client(t, c, "settings")
	var list struct {
		Skills  []library.Summary `json:"skills"`
		Curator string            `json:"curator"`
	}
	if err := sc.Call("library.list", map[string]string{"query": "token"}, &list); err != nil || len(list.Skills) != 1 || list.Skills[0].ID != "design-tokens" {
		t.Fatalf("list: %+v %v", list, err)
	}
	var entry struct {
		Versions []library.PanelVersion `json:"versions"`
	}
	if err := sc.Call("library.entry", map[string]string{"id": "fetcher"}, &entry); err != nil || len(entry.Versions) != 1 ||
		entry.Versions[0].State != library.Proposed || len(entry.Versions[0].Hits) == 0 || entry.Versions[0].Content != "" {
		t.Fatalf("entry: %+v %v", entry, err)
	}
	var ver library.Info
	if err := sc.Call("library.version", map[string]any{"id": "fetcher", "version": 1}, &ver); err != nil || !strings.Contains(ver.Content, "Fetch") {
		t.Fatalf("version: %+v %v", ver, err)
	}
	var q struct {
		Proposals []library.PanelVersion `json:"proposals"`
	}
	if err := sc.Call("library.queue", nil, &q); err != nil || len(q.Proposals) != 1 || q.Proposals[0].ID != "fetcher" {
		t.Fatalf("queue: %+v %v", q, err)
	}

	// Only a settings link reads the library: not a view-only one, nor approval, setup or agent links.
	view := client(t, c, "approve:apr_x")
	var vs struct{ Ticket string }
	if err := view.Call("session.settings", nil, &vs); err != nil {
		t.Fatal(err)
	}
	viewOnly := *view
	viewOnly.Link.Ticket = vs.Ticket
	for name, pc := range map[string]*panelclient.Client{"view-only": &viewOnly, "agent": client(t, c, "agent:Main")} {
		for _, op := range []string{"library.list", "library.entry", "library.version", "library.queue", "library.curator.set"} {
			if err := pc.Call(op, map[string]string{"id": "fetcher", "name": "Main"}, &list); err == nil {
				t.Fatalf("%s link ran %s", name, op)
			}
		}
	}
	expired := *sc
	expired.Link.Ticket = "nope"
	if err := expired.Call("library.list", nil, &list); err == nil {
		t.Fatal("no ticket listed the library")
	}

	// The curator is changed only by a strong approval.
	var res struct {
		ApprovalID string `json:"approval_id"`
		Ticket     string `json:"ticket"`
	}
	if err := sc.Call("library.curator.set", map[string]string{"name": "Ghost"}, &res); err == nil {
		t.Fatal("unknown Bot accepted")
	}
	if err := sc.Call("library.curator.set", map[string]string{"name": "main"}, &res); err != nil || res.ApprovalID == "" || res.Ticket == "" {
		t.Fatalf("curator.set: %+v %v", res, err)
	}
	if c.LibraryCurator() != "" {
		t.Fatal("curator set before approval")
	}
	snap, err := c.Approvals.Get(res.ApprovalID)
	if err != nil || snap.Kind != "rubi.library.curator" || snap.Level != approvals.Strong {
		t.Fatalf("approval: %+v %v", snap, err)
	}
	if _, err := c.Approvals.DecidePanel(context.Background(), res.ApprovalID, "apply", true, approvals.Verified); err != nil {
		t.Fatal(err)
	}
	if c.LibraryCurator() != "Main" {
		t.Fatalf("curator %q", c.LibraryCurator())
	}
	if err := sc.Call("library.list", nil, &list); err != nil || list.Curator != "Main" {
		t.Fatalf("list: %+v %v", list, err)
	}
	if err := sc.Call("library.curator.set", map[string]string{"name": "Main"}, &res); err == nil {
		t.Fatal("same curator accepted")
	}

	// No write path: these ops don't exist.
	for _, op := range []string{"library.admit", "library.submit", "library.withdraw", "library.deprecate", "library.put"} {
		if err := sc.Call(op, map[string]string{"id": "fetcher"}, &list); err == nil {
			t.Fatalf("%s exists", op)
		}
	}
	c.Lock()
	if err := sc.Call("library.list", nil, &list); err == nil {
		t.Fatal("listed while locked")
	}
}
