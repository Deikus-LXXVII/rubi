package approvals

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestInputs: typed values are needed for the items they belong to, checked before deciding, never
// accepted on paths without the panel, and reach Execute only through the context.
func TestInputs(t *testing.T) {
	c := &clock{time.Now()}
	var got map[string]string
	var runs int
	req := func() Request {
		return Request{Integration: "rubi", Kind: "rubi.x", Summary: "x",
			Options: []Option{{Key: "apply", Label: "Approve"}},
			Items:   []Item{{Key: "hook", Label: "Hook"}, {Key: "other", Label: "Other"}},
			Inputs:  []Input{{Key: "secret", Label: "Key", Secret: true, Item: "hook"}},
			CheckInputs: func(option string, in map[string]string) error {
				if !webhookLike(option) {
					return nil
				}
				if in["secret"] == "" {
					return errors.New("paste the key")
				}
				return nil
			},
			Execute: func(ctx context.Context, _ string) (any, error) {
				runs++
				got = InputsFrom(ctx)
				return nil, nil
			}}
	}
	if _, err := engine(Chat, c).Submit(context.Background(), req()); err == nil {
		t.Fatal("a request with inputs was accepted at chat level")
	}
	if _, err := engine(None, c).Submit(context.Background(), req()); err == nil {
		t.Fatal("a request with inputs ran without the panel")
	}
	e := engine(Strong, c)
	out, err := e.Submit(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	id := out["approval_id"].(string)
	if d, _ := e.Detail(id); len(d.Inputs) != 1 {
		t.Fatalf("detail inputs: %v", d.Inputs)
	}
	if _, err := e.CheckInputs(id, "apply", nil); err == nil {
		t.Fatal("missing input accepted")
	}
	if _, err := e.CheckInputs(id, "items:other", nil); err != nil {
		t.Fatalf("input needed for an item left out: %v", err)
	}
	if _, err := e.CheckInputs(id, "apply", map[string]string{"secret": "k", "evil": "x"}); err == nil {
		t.Fatal("unknown input accepted")
	}
	if s, err := e.DecidePanelWith(context.Background(), id, "apply", true, Verified, nil); err == nil || runs != 0 {
		t.Fatalf("decided without the input: %v %v", s, err)
	}
	if s, _ := e.Get(id); s.State != Pending {
		t.Fatalf("a refused input changed the state: %s", s.State)
	}
	s, err := e.DecidePanelWith(context.Background(), id, "apply", true, Verified, map[string]string{"secret": "k"})
	if err != nil || s.State != Executed || runs != 1 || got["secret"] != "k" {
		t.Fatalf("decide: %v %v runs=%d got=%v", s, err, runs, got)
	}

	plain := engine(Strong, c)
	out, _ = plain.Submit(context.Background(), Request{Kind: "x.y", Options: []Option{{Key: "ok"}},
		Execute: func(context.Context, string) (any, error) { return nil, nil }})
	if _, err := plain.DecidePanelWith(context.Background(), out["approval_id"].(string), "ok", true, Verified,
		map[string]string{"a": "b"}); err == nil {
		t.Fatal("inputs accepted by a request that takes none")
	}
}

func webhookLike(option string) bool {
	_, keys := SplitOption(option)
	if keys == nil {
		return true
	}
	for _, k := range keys {
		if k == "hook" {
			return true
		}
	}
	return false
}
