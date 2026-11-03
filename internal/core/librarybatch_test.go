package core

import (
	"context"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/library"
)

// Project rule 5 (docs/project-rules.md): withdrawing several skills from the library is one approval with an
// item each, and only the ticked ones are withdrawn.

// admitSkills puts skills with these names in the library, admitted by the curator.
func admitSkills(t *testing.T, c *Core, names ...string) {
	t.Helper()
	for _, n := range names {
		p := submitAs(t, c, "Mail", strings.Replace(libSkill, "layout-notes", n, 2))
		if _, err := c.LibraryAdmit("Main", code(c, "Main"), p.ProposalID, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func served(c *Core, id string) bool { _, err := c.LibraryGet("", "", id, 0); return err == nil }

func withdrawals(c *Core) int {
	n := 0
	for _, a := range c.Approvals.Pending() {
		if a.Kind == libraryWithdrawKind {
			n++
		}
	}
	return n
}

func TestSeveralSkillsAreWithdrawnWithOneApproval(t *testing.T) {
	c := libCore(t)
	ctx := context.Background()
	admitSkills(t, c, "skill-a", "skill-b", "skill-c")

	res, err := c.RequestLibraryWithdraws(ctx, []LibraryRef{{ID: "skill-a"}, {ID: "skill-b"}, {ID: "skill-c", Version: 1}, {ID: "no-such-skill"}})
	if err != nil {
		t.Fatal(err)
	}
	if failed, _ := res["failed"].(map[string]string); failed["no-such-skill"] == "" {
		t.Fatalf("an unknown skill is not reported: %v", res)
	}
	if n := withdrawals(c); n != 1 {
		t.Fatalf("%d withdrawal approvals wait; want 1 for three skills", n)
	}
	id := res["approval_id"].(string)
	det, _ := c.Approvals.Detail(id)
	if len(det.Items) != 3 || det.Items[2].Label != "Skill skill-c v1" {
		t.Fatalf("items: %+v", det.Items)
	}
	// each item says what would be withdrawn
	if p, _ := det.Items[0].Preview.(map[string]any); p["skill"] != "skill-a" || p["versions"] == "" || p["effect"] == nil {
		t.Fatalf("preview: %v", det.Items[0].Preview)
	}
	for _, s := range []string{"skill-a", "skill-b", "skill-c"} {
		if !served(c, s) {
			t.Fatalf("%s withdrawn before the approval", s)
		}
	}

	// skill-b is unticked: it stays.
	snap, err := c.Approvals.DecidePanel(ctx, id, "items:skill-a,skill-c@1", true, approvals.Verified)
	if err != nil || snap.State != approvals.Executed {
		t.Fatalf("approve: %+v %v", snap, err)
	}
	if served(c, "skill-a") || served(c, "skill-c") || !served(c, "skill-b") {
		t.Fatalf("served after the subset: a %v b %v c %v", served(c, "skill-a"), served(c, "skill-b"), served(c, "skill-c"))
	}
	if ev := auditFor(t, c, "library.withdrawn"); len(ev) != 2 {
		t.Fatalf("audit: %v", ev)
	}
}

func TestOneSkillThatCantBeWithdrawnDoesNotHideTheOthers(t *testing.T) {
	c := libCore(t)
	ctx := context.Background()
	admitSkills(t, c, "skill-a", "skill-b")
	res, err := c.RequestLibraryWithdraws(ctx, []LibraryRef{{ID: "skill-a"}, {ID: "skill-b"}})
	if err != nil {
		t.Fatal(err)
	}
	// The curator withdraws skill-a while the request waits: the user's step for it finds nothing to do.
	if _, err := c.Library.SetState("skill-a", 0, library.Withdrawn, "Main"); err != nil {
		t.Fatal(err)
	}
	snap, err := c.Approvals.DecidePanel(ctx, res["approval_id"].(string), "apply", true, approvals.Verified)
	if err != nil || snap.State != approvals.Executed {
		t.Fatalf("approve: %+v %v", snap, err)
	}
	out, _ := snap.Result.(map[string]any)
	failed, _ := out["failed"].(map[string]string)
	if failed["skill-a"] == "" || failed["skill-b"] != "" || served(c, "skill-b") {
		t.Fatalf("per-item report: %v; b served %v", out, served(c, "skill-b"))
	}
}

func TestOneSkillWithdrawalIsTheOrdinaryApproval(t *testing.T) {
	c := libCore(t)
	admitSkills(t, c, "skill-a")
	res, err := c.RequestLibraryWithdraw(context.Background(), "skill-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	det, _ := c.Approvals.Detail(res["approval_id"].(string))
	if len(det.Items) != 0 || det.Summary != "Withdraw skill skill-a from the library" {
		t.Fatalf("%+v", det)
	}
	if _, err := c.RequestLibraryWithdraw(context.Background(), "no-such-skill", 0); err == nil {
		t.Fatal("an unknown skill gave an approval")
	}
	if _, err := c.RequestLibraryWithdraws(context.Background(), nil); err == nil {
		t.Fatal("an empty list gave an approval")
	}
}

func TestOverlappingSkillRefsAreOneItemAndFailuresUseItemKeys(t *testing.T) {
	c := libCore(t)
	admitSkills(t, c, "skill-a", "skill-b")
	res, err := c.RequestLibraryWithdraws(context.Background(),
		[]LibraryRef{{ID: "skill-a", Version: 1}, {ID: "skill-a"}, {ID: "skill-b"}, {ID: "skill-b", Version: 9}, {ID: "no-such"}})
	if err != nil {
		t.Fatal(err)
	}
	det, _ := c.Approvals.Detail(res["approval_id"].(string))
	if len(det.Items) != 2 || det.Items[0].Key != "skill-a" || det.Items[1].Key != "skill-b" {
		t.Fatalf("items: %+v", det.Items)
	}
	if covered, _ := res["covered"].([]string); len(covered) != 2 || covered[0] != "skill-a@1" {
		t.Fatalf("covered: %v", res["covered"])
	}
	if p, _ := det.Items[0].Preview.(map[string]any); p["covers"] != "every version, including any admitted before you approve" {
		t.Fatalf("a whole-skill item does not say what it covers: %v", p)
	}
	if p, _ := det.Items[0].Preview.(map[string]any); p["skill"] != "skill-a" {
		t.Fatal(p)
	}
	// the failure is under the item's key: for a version-less ref its id, for a version ref id@version
	failed, _ := res["failed"].(map[string]string)
	if failed["no-such"] == "" || len(failed) != 1 {
		t.Fatalf("failed: %v", failed)
	}
	// a version asked for by itself says only that version
	one, err := c.RequestLibraryWithdraws(context.Background(), []LibraryRef{{ID: "skill-a", Version: 1}, {ID: "skill-b", Version: 7}})
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := one["failed"].(map[string]string); f["skill-b@7"] == "" {
		t.Fatalf("failed: %v", one["failed"])
	}
}
