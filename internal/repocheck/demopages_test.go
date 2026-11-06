package repocheck

// The panel's demo (panel/js/demo.js, opened with #demo) is how project rule 1 (phone first) is checked on every
// screen, so it has to stay complete and honest:
//
//	D1  every screen name in DEMO_SCREENS has a case in app.js demoScreen (a tile never opens the gallery again).
//	D2  the made-up T3 Agents plugin pages use only the parts of a plugin page the panel draws, stay inside the
//	    panel's limits, every button they offer is answered, and the plugin is in the demo's plugin list.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestEveryDemoScreenResolves(t *testing.T) {
	demo := readRepo(t, "panel/js/demo.js")
	app := readRepo(t, "panel/js/app.js")
	i := strings.Index(demo, "export const DEMO_SCREENS = [")
	if i < 0 {
		t.Fatal("demo.js has no DEMO_SCREENS")
	}
	block := demo[i:]
	block = block[:strings.Index(block, "\n];")]
	var names []string
	for _, m := range regexp.MustCompile(`\["([a-z0-9]+)", "[^"]+"\]`).FindAllStringSubmatch(block, -1) {
		names = append(names, m[1])
	}
	j := strings.Index(app, "function demoScreen(")
	if j < 0 || len(names) < 20 {
		t.Fatalf("expected the demo screens and app.js demoScreen; found %d names", len(names))
	}
	body := app[j:]
	body = body[:strings.Index(body, "\nfunction fatal(")]
	cases := map[string]bool{}
	for _, m := range regexp.MustCompile(`case "([a-z0-9]+)":`).FindAllStringSubmatch(body, -1) {
		cases[m[1]] = true
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("DEMO_SCREENS lists %q twice", n)
		}
		seen[n] = true
		if !cases[n] {
			t.Errorf("#demo=%s is in DEMO_SCREENS but app.js demoScreen has no case for it: its tile would show the gallery again", n)
		}
	}
	for _, n := range []string{"t3trust", "t3setup", "t3sessions", "t3chat", "t3roles", "t3ssh", "t3usage"} {
		if !seen[n] {
			t.Errorf("the T3 Agents demo screen %q is missing from DEMO_SCREENS", n)
		}
	}
}

// The parts of a plugin page the panel reads (pluginViewScreen in app.js), by level.
var pageParts = map[string][]string{
	"page":    {"title", "note", "refresh", "sections", "up"},
	"section": {"title", "note", "kind", "empty", "items", "actions", "input"},
	"item":    {"title", "detail", "warn", "actions"},
	"action":  {"label", "op", "args", "danger", "confirm", "prompt", "open"},
	"prompt":  {"arg", "label", "value"},
	"input":   {"op", "args", "arg", "label", "send"},
	"up":      {"label", "open"},
	"chat":    {"role", "author", "text", "untrusted", "time"},
}

func TestDemoT3AgentsPagesUseOnlyWhatThePanelDraws(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	app := readRepo(t, "panel/js/app.js")
	for level, keys := range pageParts {
		for _, k := range keys {
			if !regexp.MustCompile(`\.` + k + `\b`).MatchString(app) {
				t.Errorf("this test lists the %s part %q as drawn, but panel/js/app.js never reads it", level, k)
			}
		}
	}
	num := func(name string) int {
		m := regexp.MustCompile(`const ` + name + ` = (\d+);`).FindStringSubmatch(app)
		if m == nil {
			t.Fatalf("app.js has no %s", name)
		}
		n := 0
		for _, c := range m[1] {
			n = n*10 + int(c-'0')
		}
		return n
	}
	forbidden := regexp.MustCompile(`const PROMPT_FORBIDDEN = /(.*)/(\w*);`).FindStringSubmatch(app)
	if forbidden == nil {
		t.Fatal("app.js has no PROMPT_FORBIDDEN")
	}
	cfg, _ := json.Marshal(map[string]any{"parts": pageParts, "sections": num("VIEW_SECTIONS_MAX"), "items": num("VIEW_ITEMS_MAX"),
		"actions": num("VIEW_ACTIONS_MAX"), "text": num("VIEW_TEXT_MAX"), "forbidden": forbidden[1], "flags": forbidden[2],
		"chatRoles": []string{"user", "assistant", "tool", "sister"}})

	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join(root(t), "panel/js/demo.js"))
	if err != nil {
		t.Fatal(err)
	}
	mod := filepath.Join(dir, "demo.mjs")
	if err := os.WriteFile(mod, src, 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
import * as m from ` + fileURLJS(mod) + `;
const cfg = JSON.parse(process.argv.at(-1));
const bad = [];
const forbidden = new RegExp(cfg.forbidden, cfg.flags);
const only = (where, level, o) => { for (const k of Object.keys(o)) if (!cfg.parts[level].includes(k)) bad.push(where + ": the " + level + " part " + JSON.stringify(k) + " is not drawn by the panel"); };
const plain = (x) => x && typeof x === "object" && !Array.isArray(x);
function check(where, page) {
  if (!plain(page)) return bad.push(where + ": not an object");
  only(where, "page", page);
  if (page.up !== undefined) { only(where, "up", page.up); if (!plain(page.up.open)) bad.push(where + ": up has no open"); }
  if (!Array.isArray(page.sections) || page.sections.length > cfg.sections) bad.push(where + ": sections missing or over " + cfg.sections);
  const action = (a, w) => {
    only(w, "action", a);
    if (typeof a.label !== "string" || !a.label || a.label.length > 60) bad.push(w + ": a button label is empty or over 60 characters: " + a.label);
    if (!a.open && !a.op) bad.push(w + ": button " + a.label + " has neither open nor op");
    if (a.prompt) {
      only(w, "prompt", a.prompt);
      if (!a.prompt.arg || forbidden.test(a.prompt.label || "")) bad.push(w + ": the panel won't show the question of " + a.label);
      if ((a.prompt.label || "").length > 200) bad.push(w + ": a question over 200 characters");
    }
  };
  for (const s of page.sections || []) {
    const w = where + " / " + s.title;
    only(w, "section", s);
    if (s.kind !== undefined && s.kind !== "chat") bad.push(w + ": unknown section kind " + s.kind);
    if (s.actions && s.actions.length > cfg.actions) bad.push(w + ": too many section buttons");
    for (const a of s.actions || []) action(a, w);
    if (s.kind === "chat") {
      only(w, "input", s.input);
      for (const c of s.items) { only(w, "chat", c); if (!cfg.chatRoles.includes(c.role)) bad.push(w + ": chat role " + c.role); }
      continue;
    }
    if (s.items !== undefined && (!Array.isArray(s.items) || s.items.length > cfg.items)) bad.push(w + ": items not a list or over " + cfg.items);
    for (const it of s.items || []) {
      only(w, "item", it);
      if (!it.title) bad.push(w + ": an item without a title");
      if ((it.detail || "").length > cfg.text) bad.push(w + " / " + it.title + ": a detail over " + cfg.text + " characters (the panel cuts it)");
      if ((it.actions || []).length > cfg.actions) bad.push(w + " / " + it.title + ": too many buttons");
      for (const a of it.actions || []) action(a, w + " / " + it.title);
    }
  }
}
const buttons = (page) => (page.sections || []).flatMap((s) => [...(s.actions || []), ...(s.items || []).flatMap((i) => i.actions || [])]);
const sample = (op, arg) => ({ role_new: "demo-role", role_convert: "demo-convert", ssh_add: "box host.example.com deploy", ssh_draft_cmd: "uptime", role_deny_add: "WebFetch",
  setup_code: "abc123", rename: "A new name" }[op] || "hello");
const unknown = (e) => /^unknown (op|page)/.test(String(e.message));
const page = async (args) => m.demoT3Page("view", args);
// pass 1: every page reachable by an open button, as it is first shown
const seen = new Map(), queue = [{}];
while (queue.length) {
  const args = queue.shift(), key = JSON.stringify(args);
  if (seen.has(key)) continue;
  let p;
  try { p = await page(args); } catch (e) { bad.push("opening " + key + ": " + e.message); seen.set(key, null); continue; }
  seen.set(key, p);
  check(key, p);
  for (const a of buttons(p)) if (a.open) queue.push(a.open);
  if (p.up) queue.push(p.up.open);
}
// the steps that take time: the sign-in shows a link, then asks for the code, then is done; a self-test works and ends with ok lines
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const setup = { page: "setup", device: "dev_mac" };
const titles = () => JSON.stringify(m.demoT3Page("view", setup));
check("sign-in start", m.demoT3Page("view", setup));
m.demoT3Page("setup_run", { device: "dev_mac", step: "login", method: "claudeai", project: "reviewer" });
await sleep(1700);
check("sign-in link", m.demoT3Page("view", setup));
if (!titles().includes("Open the sign-in page") || !titles().includes("Paste the code")) bad.push("the sign-in shows no link and code box");
m.demoT3Page("setup_open", { device: "dev_mac" });
if (!titles().includes("Claude is waiting for the code")) bad.push("the sign-in does not ask for the code after the link was opened");
m.demoT3Page("setup_code", { device: "dev_mac", code: "abc123" });
await sleep(2700);
if (!titles().includes("Signed in with a Claude subscription") || titles().includes("Paste the code")) bad.push("the sign-in does not end");
m.demoT3Page("setup_run", { device: "dev_mac", step: "selftest", project: "web-app" });
if (!titles().includes("Working on: Self-test")) bad.push("the self-test does not show that it works");
await sleep(4200);
check("self-test done", m.demoT3Page("view", setup));
if (!titles().includes("ok: ")) bad.push("the self-test shows no ok lines");
// pass 2: every other button is pressed (with a sample answer for its question) and the page is drawn again
let pressed = 0;
for (const [key, p] of seen) {
  if (!p) continue;
  for (const a of buttons(p)) {
    if (!a.op) continue;
    let args = a.args || {};
    if (a.prompt) args = { ...args, [a.prompt.arg]: sample(a.op, a.prompt.arg) };
    try { m.demoT3Page(a.op, args); pressed++; } catch (e) { if (unknown(e)) bad.push(key + ": nothing answers " + a.op); }
    try { check(key + " after " + a.op, m.demoT3Page("view", JSON.parse(key))); } catch (e) { if (unknown(e)) bad.push(key + ": " + e.message); }
  }
}
// the plugin is in the plugin list, with a page
const list = await new m.DemoClient().call("store.list");
const me = list.plugins.find((x) => x.id === "t3agents");
const pluginOK = !!me && me.installed && me.connected && me.panel === "view" && me.name === "T3 Agents";
console.log(JSON.stringify({ pages: seen.size, pressed, bad, pluginOK, names: [...seen.keys()].length }));
process.exit(0);
`
	cmd := exec.Command(node, "--input-type=module", "-e", script, "--", string(cfg))
	cmd.Dir = dir
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var got struct {
		Pages, Pressed int
		Bad            []string
		PluginOK       bool
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	sort.Strings(got.Bad)
	for _, b := range got.Bad {
		t.Error(b)
	}
	if got.Pages < 12 || got.Pressed < 60 {
		t.Errorf("the walk saw %d pages and pressed %d buttons; the T3 Agents demo should have well over that", got.Pages, got.Pressed)
	}
	if !got.PluginOK {
		t.Error("the demo's plugin list needs an installed, connected T3 Agents entry (id t3agents, panel \"view\") so Settings shows it")
	}
}
