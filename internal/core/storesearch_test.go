package core

import (
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

func testCatalog() *plugins.Catalog {
	cat := &plugins.Catalog{Plugins: []plugins.CatalogEntry{
		{ID: "gmail", Name: "Gmail", Summary: "Read and send Gmail email.", Versions: []plugins.CatalogVersion{{Version: "v1.2.0", MinRubi: "v0.9.0"}}},
		{ID: "hue", Name: "Philips Hue", Summary: "Lights at home.", Versions: []plugins.CatalogVersion{{Version: "v0.3.0"}, {Version: "v0.4.0", MinRubi: "v99.0.0"}}},
		{ID: "telegram", Name: "Telegram", Summary: "Messages.", Versions: []plugins.CatalogVersion{{Version: "v1.0.0"}}},
	}}
	return cat
}

func TestSearchCatalog(t *testing.T) {
	cat := testCatalog()
	installed := func(id string) (plugins.Manifest, bool) {
		if id != "telegram" {
			return plugins.Manifest{}, false
		}
		m := plugins.Manifest{ID: "telegram", Description: "Your Telegram account", Egress: []string{"api.telegram.org:443"}}
		m.Actions = append(m.Actions, struct2action("telegram.send", "Send message"))
		return m, true
	}
	ids := func(q StoreQuery) []string {
		var out []string
		for _, it := range searchCatalog(cat, installed, q, "v0.11.0") {
			out = append(out, it["id"].(string))
		}
		return out
	}
	eq := func(got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}
	eq(ids(StoreQuery{Query: "GMAIL"}), "gmail")
	eq(ids(StoreQuery{Query: "email send"}), "gmail")       // every word, in any field
	eq(ids(StoreQuery{Query: "send"}), "gmail", "telegram") // telegram by its declared action
	eq(ids(StoreQuery{Query: "nothing-like-this"}))
	eq(ids(StoreQuery{Capability: "telegram.send"}), "telegram")
	eq(ids(StoreQuery{Capability: "lights"}), "hue") // not installed: only its store text can match
	eq(ids(StoreQuery{Query: "telegram", Capability: "lights"}))

	all := searchCatalog(cat, installed, StoreQuery{Query: "e"}, "v0.11.0")
	for _, it := range all {
		switch it["id"] {
		case "gmail":
			if it["latest"] != "v1.2.0" || it["min_rubi"] != "v0.9.0" {
				t.Fatalf("gmail: %v", it)
			}
		case "hue": // the newest version needs a newer Rubi: the one this Rubi can run is offered
			if it["latest"] != "v0.3.0" {
				t.Fatalf("hue: %v", it)
			}
		case "telegram":
			n, ok := it["needs"].(map[string]any)
			if !ok || len(n["actions"].([]map[string]any)) != 1 || n["connects_to"].([]string)[0] != "api.telegram.org:443" {
				t.Fatalf("telegram needs: %v", it["needs"])
			}
		}
	}
}

func struct2action(kind, title string) (a rubiplugin.Action) {
	a.Kind, a.Title, a.DefaultLevel = kind, title, "strong"
	return a
}
