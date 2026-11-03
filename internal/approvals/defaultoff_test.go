package approvals

import (
	"context"
	"strings"
	"testing"
	"time"
)

// An item marked DefaultOff is not part of "all": the option key, level none and a chat button leave it out, and it
// is decided on only when its key is named in items:.
func TestDefaultOffItemsNeedToBeNamed(t *testing.T) {
	var got []string
	req := func() Request {
		return Request{Integration: "rubi", Kind: "rubi.plugin.install", Summary: "Install 3",
			Options: []Option{{Key: "install", Label: "Install"}},
			Items:   []Item{{Key: "a", Label: "A"}, {Key: "b", Label: "B", DefaultOff: true}, {Key: "c", Label: "C"}},
			Execute: func(_ context.Context, opt string) (any, error) {
				got = append(got, opt)
				return nil, nil
			}}
	}
	c := &clock{time.Now()}
	e := engine(Strong, c)
	ctx := context.Background()

	r := req()
	if opt, err := r.normalize("install"); err != nil || opt != "items:a,c" {
		t.Fatalf("the option key stands for the items that are on: %q %v", opt, err)
	}
	if opt, err := r.normalize("items:a,b"); err != nil || opt != "items:a,b" {
		t.Fatalf("a named default-off item is chosen: %q %v", opt, err)
	}
	out, _ := e.Submit(ctx, req())
	if snap, err := e.DecidePanel(ctx, out["approval_id"].(string), "install", true, Verified); err != nil || snap.State != Executed || got[0] != "items:a,c" {
		t.Fatalf("approve: %+v %v got %v", snap, err, got)
	}
	out, _ = e.Submit(ctx, req())
	if _, err := e.DecidePanel(ctx, out["approval_id"].(string), "items:b", true, Verified); err != nil || got[1] != "items:b" {
		t.Fatalf("ticked on purpose: %v %v", err, got)
	}
	// level none runs the same selection
	got = nil
	if _, err := engine(None, c).Submit(ctx, req()); err != nil || got[0] != "items:a,c" {
		t.Fatalf("level none: %v %v", err, got)
	}
	// a request whose items are all off has no "all"
	all := req()
	all.Items = []Item{{Key: "b", Label: "B", DefaultOff: true}}
	if _, err := all.normalize("install"); err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("all off: %v", err)
	}
}
