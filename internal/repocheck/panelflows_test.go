package repocheck

// Project rule 1 (docs/project-rules.md), second half: everything that needs the user has a screen in the panel
// that can finish it. The lists below are read from the Go code, so a new flow without a screen fails here:
//
//	F1  every purpose of a panel link that core can mint (Core.validPurpose) has a route in panel/js/app.js main().
//	F2  everything an approval can ask the user for (the fields of approvals.Detail, the proof levels, the proofs)
//	    is rendered or collected by the panel.
//	F3  the panel names no approval kind that no code raises (a screen left behind), and every kind core raises
//	    is finished by the generic approval screen (F2) or by a screen of its own.
//	F4  every RPC the panel server answers (panelapi dispatch) has a caller in the panel's scripts.
//	F5  every kind of field and every part of a plugin page that the plugin SDK documents is drawn by the panel.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func readRepo(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func panelJS(t *testing.T) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(root(t), "panel/js/*.js"))
	var sb strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "demo.js") { // made-up data: it is not the panel
			continue
		}
		b, _ := os.ReadFile(f)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// stringsIn returns the string literals of the function named fn in a Go file.
func stringsIn(t *testing.T, rel, fn string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root(t), rel), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn && fd.Body != nil {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
					if v, err := strconv.Unquote(bl.Value); err == nil {
						out = append(out, v)
					}
				}
				return true
			})
		}
	}
	return out
}

// jsonTags returns the json names of the fields of a struct type in a Go file ("-" and untagged fields left out).
func jsonTags(t *testing.T, rel, typ string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root(t), rel), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != typ {
			return true
		}
		if st, ok := ts.Type.(*ast.StructType); ok {
			for _, fl := range st.Fields.List {
				if fl.Tag == nil {
					continue
				}
				tag, _ := strconv.Unquote(fl.Tag.Value)
				if m := regexp.MustCompile(`json:"([^",]+)`).FindStringSubmatch(tag); m != nil && m[1] != "-" {
					out = append(out, m[1])
				}
			}
		}
		return false
	})
	if len(out) == 0 {
		t.Fatalf("no json fields found for %s in %s", typ, rel)
	}
	return out
}

func TestEveryPanelLinkHasARoute(t *testing.T) {
	js := readRepo(t, "panel/js/app.js")
	var purposes []string
	seen := map[string]bool{}
	for _, s := range stringsIn(t, "internal/core/core.go", "validPurpose") {
		if regexp.MustCompile(`^[a-z]+:?$`).MatchString(s) && !seen[s] {
			seen[s] = true
			purposes = append(purposes, s)
		}
	}
	if len(purposes) < 6 {
		t.Fatalf("found the link purposes %v in core.validPurpose; expected pair, unlock, settings, setup:, approve:, agent:", purposes)
	}
	for _, p := range purposes {
		route := `link.a === "` + p + `"`
		if strings.HasSuffix(p, ":") {
			route = `link.a.startsWith("` + p + `")`
		}
		// "settings" and "unlock" are also reached through the status screen, but the entry point is main()
		if !strings.Contains(js, route) {
			t.Errorf("core can mint a panel link for %q, but panel/js/app.js main() has no route %s\n\tproject rule 1: every flow that needs the user needs a screen", p, route)
		}
	}
}

func TestApprovalScreenCollectsEverythingAnApprovalAsks(t *testing.T) {
	js := panelJS(t)
	// the fields of an approval the panel must draw or send back
	fields := append(jsonTags(t, "internal/approvals/approvals.go", "Snapshot"), jsonTags(t, "internal/approvals/approvals.go", "Detail")...)
	drawn := map[string]string{ // json field -> how the panel reads it
		"summary": `\.summary`, "question": `\.question`, "preview": `\.preview`, "options": `\.options`, "items": `\.items`,
		"inputs": `\.inputs`, "state": `\.state`, "error": `\.error`, "expires_at": `\.expires_at`, "kind": `\.kind`,
		"result": `\.result`, "level": `\.level`,
	}
	// not shown on the approval screen by design: the id is the link's, the plugin name is in the summary, the code is for chat
	notShown := map[string]string{"approval_id": "carried by the link and the ticket", "integration": "the summary and preview name it", "code": "the chat level's confirmation; not a panel level",
		"chosen_option": "the server's record of the answer; the panel sends the choice and does not read it back"}
	for _, f := range fields {
		if notShown[f] != "" {
			continue
		}
		re, ok := drawn[f]
		if !ok {
			t.Errorf("approvals.Detail/Snapshot has the field %q, which this test does not know: add it to drawn (with how the panel reads it) or to notShown (with why)", f)
			continue
		}
		if !regexp.MustCompile(re).MatchString(js) {
			t.Errorf("approvals.Detail has the field %q, but the panel's scripts never read it (%s)\n\tproject rule 1: a request asking for it could not be answered on a phone", f, re)
		}
	}
	// the input kinds: a request can ask for plain and secret input (approvals.Input.Secret)
	for _, tag := range jsonTags(t, "internal/approvals/approvals.go", "Input") {
		if !regexp.MustCompile(`\.` + regexp.QuoteMeta(tag) + `\b|"` + regexp.QuoteMeta(tag) + `"`).MatchString(js) {
			t.Errorf("approvals.Input has the field %q, but the panel never reads it", tag)
		}
	}
	// the levels decided in the panel, and the ways to prove oneself
	for _, lvl := range []string{"tap", "strong"} {
		if !strings.Contains(js, `"`+lvl+`"`) {
			t.Errorf("approvals can be at level %q, but the panel does not know it", lvl)
		}
	}
	for _, op := range []string{"approval.get", "approval.decide", "approval.challenge", "approval.list", "approval.open"} {
		if !strings.Contains(js, `"`+op+`"`) {
			t.Errorf("the panel never calls %s: waiting requests could not be listed, opened or answered", op)
		}
	}
	for _, kind := range []string{"passkey", "password"} {
		if !strings.Contains(js, `"`+kind+`"`) {
			t.Errorf("the panel cannot prove with a %s", kind)
		}
	}
}

// kindRE matches the approval kinds core raises itself.
var kindRE = regexp.MustCompile(`^rubi(\.[a-z_]+)+$`)

func TestPanelKnowsOnlyKindsThatExist(t *testing.T) {
	base := root(t)
	raised := map[string]bool{}
	fset := token.NewFileSet()
	files, _ := filepath.Glob(filepath.Join(base, "internal/core/*.go"))
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				if v, err := strconv.Unquote(bl.Value); err == nil && kindRE.MatchString(v) {
					raised[v] = true
				}
			}
			return true
		})
	}
	if len(raised) < 8 {
		t.Fatalf("found only the kinds %v in internal/core", raised)
	}
	js := panelJS(t)
	named := map[string]bool{}
	for _, m := range regexp.MustCompile(`"(rubi(?:\.[a-z_]+)+)"`).FindAllStringSubmatch(js, -1) {
		named[m[1]] = true
	}
	var gone []string
	for k := range named {
		if !raised[k] {
			gone = append(gone, k)
		}
	}
	sort.Strings(gone)
	for _, k := range gone {
		t.Errorf("panel/js names the approval kind %q, which no code in internal/core raises: remove the screen code", k)
	}
	// the kinds core raises that the panel has special words for are the user-visible ones; the others use the
	// generic screen (summary, preview, options, items, inputs), which TestApprovalScreenCollectsEverythingAnApprovalAsks holds to account
	if !strings.Contains(js, "approveScreen") {
		t.Error("the panel has no approval screen")
	}
}

func TestEveryPanelRPCHasACaller(t *testing.T) {
	src := readRepo(t, "internal/panelapi/panelapi.go")
	js := panelJS(t)
	// ops answered but never called by this panel, with the reason
	unused := map[string]string{
		"webhook.set": "kept for panels older than the agents list (agent.add replaced it)",
	}
	ops := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\tcase ((?:"[a-z]+(?:\.[a-z]+)*"(?:, )?)+):`).FindAllStringSubmatch(src, -1) {
		for _, w := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1) {
			ops[w[1]] = true
		}
	}
	for _, m := range regexp.MustCompile(`(?m)^\t\tcase ((?:"[a-z]+\.[a-z.]+"(?:, )?)+):`).FindAllStringSubmatch(src, -1) {
		for _, w := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1) {
			ops[w[1]] = true
		}
	}
	if len(ops) < 40 {
		t.Fatalf("found %d ops in panelapi.go; the test no longer reads the dispatch", len(ops))
	}
	var list []string
	for o := range ops {
		list = append(list, o)
	}
	sort.Strings(list)
	for _, o := range list {
		if strings.Contains(js, `"`+o+`"`) || unused[o] != "" {
			continue
		}
		t.Errorf("the panel server answers %q, but no script of the panel calls it\n\tproject rule 1: a flow behind it has no screen (or the op is dead: remove it)", o)
	}
	for o, why := range unused {
		if strings.Contains(js, `"`+o+`"`) {
			t.Errorf("%q is listed as unused (%s), but the panel calls it: remove it from the list", o, why)
		}
		if !ops[o] {
			t.Errorf("%q is listed as unused (%s), but the server no longer answers it: remove it from the list", o, why)
		}
	}
}

func TestPanelDrawsEverythingAPluginPageCanSay(t *testing.T) {
	js := readRepo(t, "panel/js/app.js")
	sdk := readRepo(t, "sdk/rubiplugin/manifest.go")
	// the parts of an "view" page and of a button, as the manifest documents them
	for _, k := range []string{"title", "note", "refresh", "sections", "empty", "items", "detail", "actions", "label", "op", "args", "danger", "confirm", "prompt", "folder", "open", "up", "chat", "input"} {
		doc := k
		if k == "chat" {
			doc = `kind "chat"`
		}
		if k == "open" || k == "up" || k == "input" || k == "chat" {
			// documented in the panel's own header comment rather than the manifest's
			if !strings.Contains(js, doc) {
				t.Errorf("plugin pages can say %q, but panel/js/app.js never draws it", k)
			}
			continue
		}
		if !strings.Contains(sdk, k) {
			t.Errorf("the test expects the SDK to document the page part %q", k)
		}
		if !regexp.MustCompile(`\.` + k + `\b`).MatchString(js) {
			t.Errorf("plugin pages can say %q (sdk/rubiplugin/manifest.go), but panel/js/app.js never reads it\n\tproject rule 1: a step of a plugin page could not be completed", k)
		}
	}
	// the types of setup fields and settings: those the manifest names must be drawn
	typeDoc := regexp.MustCompile(`Type\s+string\s+` + "`" + `json:"type"` + "`" + `[^\n]*|// Type is [^\n]*(\n\t//[^\n]*)*`)
	var seen []string
	for _, d := range typeDoc.FindAllString(sdk, -1) {
		for _, m := range regexp.MustCompile(`"([a-z]+)"`).FindAllStringSubmatch(d, -1) {
			seen = append(seen, m[1])
		}
	}
	if len(seen) < 6 {
		t.Fatalf("found the field types %v in the manifest; expected text, email, tel, number, choice, bool, list, info", seen)
	}
	for _, ty := range seen {
		if ty == "type" {
			continue
		}
		if ty == "text" { // the default input
			continue
		}
		if !regexp.MustCompile(`(===|case|==) *"` + ty + `"|\b` + ty + `: *"` + ty + `"`).MatchString(js) {
			t.Errorf("the manifest has the field type %q, but panel/js/app.js has no code for it", ty)
		}
	}
}

// screen() may put a new page in place a moment later (inside a view transition), so an element made for that
// page is not in the document yet when screen() returns. A loop that runs "while the element is on the page"
// ends before its first round: the hook progress screen never showed the result that way. Such a loop waits
// first and then asks whether the user has left the page.
var whileConnected = regexp.MustCompile(`while\s*\([^)]*\.isConnected`)

func TestNoPanelLoopStopsBeforeItsPageIsShown(t *testing.T) {
	if !whileConnected.MatchString("  while (status.isConnected && Date.now() < until) {") {
		t.Fatal("the pattern no longer matches the loop it was written for")
	}
	files, _ := filepath.Glob(filepath.Join(root(t), "panel/js/*.js"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if whileConnected.MatchString(line) {
				t.Errorf("%s:%d: a loop conditioned on isConnected stops before screen() has shown its page; wait first, then check", filepath.Base(f), i+1)
			}
		}
	}
}

// A settings section is a list of nodes in which a part that doesn't apply is null (`cond ? h(...) : null`).
// Element.replaceChildren/append write null as the text "null", so the list is filtered first: the Rubi Home
// section showed "nullnull" when no pairing code was waiting.
func TestSettingsSectionsDropTheirNullParts(t *testing.T) {
	js := readRepo(t, "panel/js/app.js")
	if !strings.Contains(js, "...sections[id]()") {
		t.Fatal("the settings screen no longer draws sections[id](); update this check")
	}
	if strings.Contains(js, "replaceChildren(...sections[id]())") || !strings.Contains(js, "...sections[id]().filter(") {
		t.Error("settings sections are drawn without dropping their null parts; null would show as the text \"null\"")
	}
}

// A feature the computer does not have is drawn as a calm muted line after a check of that computer, never in the error style,
// never with a button, and the data comes from what core passes on from Rubi Home's hello.
func TestPanelShowsUnavailableFeaturesCalmly(t *testing.T) {
	js := readRepo(t, "panel/js/app.js")
	i := strings.Index(js, "function featureLines(")
	if i < 0 {
		t.Fatal("the panel has no featureLines")
	}
	body := js[i:]
	if j := strings.Index(body, "\n}\n"); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, `"muted small"`) || strings.Contains(body, "error") || strings.Contains(body, "button") || strings.Contains(body, "danger") {
		t.Errorf("featureLines is not a calm muted line:\n%s", body)
	}
	if !strings.Contains(body, "not available on this computer") {
		t.Error("featureLines does not say 'not available on this computer'")
	}
	if !strings.Contains(js, "...featureLines(r.features)") {
		t.Error("the device check does not draw featureLines")
	}
}
