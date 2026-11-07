package panelapi

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPanelParseWebhook runs the panel's paste parser (panel/js/webhook.js) under Node on what users
// actually paste, and on hostile input. Skipped without node.
func TestPanelParseWebhook(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	const (
		u = "https://api2.cursor.sh/automations/webhook/6076a0ff-5b26-405e-82ef-d6173cd96066"
		k = "crsr_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	type want struct {
		URL, Key string
		Routine  bool
		Err      bool
	}
	ok := want{URL: u, Key: k, Routine: true}
	tests := []struct {
		name   string
		in     string
		strict bool
		want   want
	}{
		{"both fields", "POST to\n" + u + "\nheader\nAuthorization: Bearer " + k, true, ok},
		{"header first", "Authorization: Bearer " + k + "\n" + u, true, ok},
		{"without Bearer", u + " " + k, true, ok},
		{"whole webhook section", "Webhook\nPOST to\n" + u + "\nCopy\nkey\n" + k + "\nCopy\nheader\nAuthorization: Bearer " + k + "\nRegenerate key", true, ok},
		{"curl command", "curl -X POST \"" + u + "\" \\\n  -H \"Authorization: Bearer " + k + "\" \\\n  -H \"Content-Type: application/json\" \\\n  -d '{\"event\":\"test\"}'", true, ok},
		{"extra whitespace", "\r\n\t  " + u + "  \r\n\r\n   Bearer   " + k + " \t\n", true, ok},
		{"trailing punctuation", "POST to " + u + ". Header: Bearer " + k + ".", true, ok},
		{"address only", u, true, want{URL: u}},
		{"header only", "Authorization: Bearer " + k, true, want{Key: k}},
		{"empty", "", true, want{}},
		{"other host", "https://evil.example/automations/webhook/x " + k, true, want{Err: true}},
		{"other host, settings", "https://evil.example/automations/webhook/x " + k, false, want{URL: "https://evil.example/automations/webhook/x", Key: k}},
		{"look-alike host", "https://api2.cursor.sh.evil.example/automations/webhook/x " + k, true, want{Err: true}},
		{"userinfo trick", "https://api2.cursor.sh@evil.example/automations/webhook/x " + k, true, want{Err: true}},
		{"userinfo trick, settings", "https://api2.cursor.sh@evil.example/automations/webhook/x " + k, false, want{Err: true}},
		{"http", "http://api2.cursor.sh/automations/webhook/x " + k, true, want{Err: true}},
		{"http, settings", "http://hooks.example/x " + k, false, want{Err: true}},
		{"query on the routine", u + "?next=https://evil.example " + k, true, want{Err: true}},
		{"javascript", "javascript:alert(1) " + k, true, want{Key: k}},
		{"huge", u + " " + k + strings.Repeat(" x", 5000), true, want{Err: true}},
		{"header injection", u + "\nBearer crsr_abcdefgh12\r\nX-Evil: 1", true, want{URL: u, Key: "crsr_abcdefgh12", Routine: true}},
		{"two routines", u + " " + u[:len(u)-1] + "7 " + k, true, want{Err: true}},
		{"two keys", u + " " + k + " crsr_ffffffffffffffff", true, want{Err: true}},
		{"other receiver's token, settings", "https://hooks.example/x\nAuthorization: Bearer s3cret-token-0123456789", false, want{URL: "https://hooks.example/x", Key: "s3cret-token-0123456789"}},
		{"other receiver's token, Connect link", "https://hooks.example/x\nAuthorization: Bearer s3cret-token-0123456789", true, want{Err: true}},
		{"a token that isn't a routine key", u + " Bearer s3cret-token-0123456789", true, want{URL: u}},
	}

	type in struct {
		Text   string `json:"text"`
		Strict bool   `json:"strict"`
	}
	ins := make([]in, len(tests))
	for i, tt := range tests {
		ins[i] = in{tt.in, tt.strict}
	}
	mod, _ := filepath.Abs("../../panel/js/webhook.js")
	script := `import { parseWebhook } from ` + strconvQuote("file://"+mod) + `;
let s = ""; process.stdin.on("data", (d) => { s += d; });
process.stdin.on("end", () => { console.log(JSON.stringify(JSON.parse(s).map((x) => parseWebhook(x.text, { strict: x.strict })))); });`
	body, _ := json.Marshal(ins)
	cmd := exec.Command(node, "--input-type=module", "-e", script)
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	outb, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.Bytes())
	}
	var got []struct {
		URL     string `json:"url"`
		Key     string `json:"key"`
		Routine bool   `json:"routine"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(outb, &got); err != nil || len(got) != len(tests) {
		t.Fatalf("node output: %v\n%s", err, outb)
	}
	for i, tt := range tests {
		g := got[i]
		if tt.want.Err != (g.Error != "") {
			t.Errorf("%s: error %q, want error %v", tt.name, g.Error, tt.want.Err)
			continue
		}
		if !tt.want.Err && (g.URL != tt.want.URL || g.Key != tt.want.Key || g.Routine != tt.want.Routine) {
			t.Errorf("%s: got url %q key %q routine %v, want %+v", tt.name, g.URL, g.Key, g.Routine, tt.want)
		}
		if strings.Contains(g.Error, k) {
			t.Errorf("%s: the error repeats the key", tt.name)
		}
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// TestPanelPluginPageRefresh runs the plugin page's refresh (panel/js/viewrefresh.js) under Node the way the page
// uses it: every draw arms a refresh, and a click draws the page again. The page used to start one more reload loop
// with every click and refresh, until it redrew itself all the time and wiped what the user was doing. Skipped
// without node.
func TestPanelPluginPageRefresh(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	mod, _ := filepath.Abs("../../panel/js/viewrefresh.js")
	script := `import { createRefresher, unchanged } from ` + strconvQuote("file://"+mod) + `;
let pending = [];
const r = createRefresher((f) => { const t = { f }; pending.push(t); return t; }, (t) => { pending = pending.filter((x) => x !== t); });
let draws = 0;
const draw = () => { draws++; r.arm(3, draw); };
draw();
for (let i = 0; i < 5; i++) draw(); // five clicks
const afterClicks = draws;
for (let round = 0; round < 10; round++) { const now = pending; pending = []; now.forEach((t) => t.f()); }
const waiting = pending.length;
r.cancel();
const now = pending; pending = []; now.forEach((t) => t.f());
console.log(JSON.stringify({ byRefresh: draws - afterClicks, waiting, afterCancel: pending.length,
  same: unchanged("k", "{}", "k", "{}"), other: unchanged("k", "{}", "j", "{}"), changed: unchanged("k", "{}", "k", "{\"a\":1}") }));`
	out, err := exec.Command(node, "--no-warnings", "--input-type=module", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		ByRefresh, Waiting, AfterCancel int
		Same, Other, Changed            bool
	}
	if err := json.Unmarshal(jsonLine(out), &got); err != nil {
		t.Fatalf("node output: %v\n%s", err, out)
	}
	if got.ByRefresh != 10 || got.Waiting != 1 || got.AfterCancel != 0 {
		t.Errorf("10 refresh rounds after 5 clicks drew %d times with %d refreshes waiting (want 10 and 1); %d after cancel", got.ByRefresh, got.Waiting, got.AfterCancel)
	}
	if !got.Same || got.Other || got.Changed {
		t.Errorf("unchanged: %+v", got)
	}
	// The page arms its refresh only through the refresher: a bare timer would start a loop of its own again.
	app, err := os.ReadFile("../../panel/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(app), "setTimeout(tick") {
		t.Error("panel/js/app.js arms a refresh with a bare setTimeout")
	}
}

// jsonLine is the last line of node output that is JSON. A warning on stderr mixed into
// combined output must not make the parse fail.
func jsonLine(out []byte) []byte {
	var last []byte
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) > 0 && line[0] == '{' {
			last = line
		}
	}
	if last == nil {
		return out
	}
	return last
}
