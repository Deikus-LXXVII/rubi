package mcpserver

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPluginWantInAcceptsStringsAndObjects(t *testing.T) {
	var in agentConnectIn
	if err := json.Unmarshal([]byte(`{"name":"Mail","plugins":["gmail",{"id":"icloud-mail","accounts":["a@b.c"]},{"id":"demo"}]}`), &in); err != nil {
		t.Fatal(err)
	}
	want := []pluginWantIn{{ID: "gmail"}, {ID: "icloud-mail", Accounts: []string{"a@b.c"}}, {ID: "demo"}}
	if !reflect.DeepEqual(in.Plugins, want) {
		t.Fatalf("got %+v", in.Plugins)
	}
	for _, bad := range []string{`{"plugins":[5]}`, `{"plugins":[true]}`, `{"plugins":[["gmail"]]}`, `{"plugins":[{"id":3}]}`, `{"plugins":[null,5]}`} {
		var x agentConnectIn
		if err := json.Unmarshal([]byte(bad), &x); err == nil && (len(x.Plugins) != 1 || x.Plugins[0].ID != "") {
			t.Fatalf("%s accepted: %+v", bad, x.Plugins)
		}
	}
}

func TestPluginsSchemaDeclaresStringOrObject(t *testing.T) {
	for name, sc := range map[string]any{"connect": pluginsAcceptStrings[agentConnectIn](), "request": pluginsAcceptStrings[accessRequestIn]()} {
		b, _ := json.Marshal(sc)
		s := string(b)
		if !strings.Contains(s, `"anyOf":[{"type":"string"`) || !strings.Contains(s, `"accounts"`) {
			t.Fatalf("%s schema: %s", name, s)
		}
	}
}
