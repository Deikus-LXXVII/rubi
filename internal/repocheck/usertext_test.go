package repocheck

// Project rule 2 (docs/project-rules.md): the user is never made to use a terminal, neither to use nor to set up
// Rubi. This test reads every text the user can come to see (what Rubi returns to Bots and what Bots relay, the
// panel's own strings, the user docs) and refuses the ones that tell the user to run a command.
//
// What counts as user-facing:
//   - Go: every string literal of the non-test sources that holds a space (so it is words, not a program name or a
//     flag): next_step, instructions, hints, messages, error texts and the lines that go to a log.
//   - panel/js/*.js: every string and template literal.
//   - docs/user/*.md: the whole file.
//
// What is exempt by path (userTextExempt): code and docs that are maintainer-facing, and texts addressed to the Bot
// (skills, MCP tool descriptions and results, rubi --help). Project rule 2 allows commands the Bot runs on the
// user's computer while the user types nothing, so a text the Bot reads may hold a command.
//
// In text the user reads, a command line is allowed only when its own sentence is handed to the Bot (see
// botAddressed): "Tell your Bot: ...", "Ask your Bot to ...", "Your Bot runs ...". The sentence is the unit, so a
// Bot phrase elsewhere in the paragraph does not cover a command addressed to the user, and a sentence that names
// a terminal, a prompt, or "you" doing something is never covered.
//
// Existing violations are listed in testdata/usertext_allow.txt with a task reference each. A new violation fails,
// and so does an entry that no longer matches anything: the list only ever shrinks.

import (
	"bufio"
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

// userTextExempt are paths whose texts are for the people who build, release and host Rubi, never for its users.
var userTextExempt = map[string]string{
	"docs/ops/":                    "maintainer docs: hosting the gateway, releasing, signing plugins",
	"cmd/t3-bump/":                 "CI tool that opens the T3 pin pull request",
	"internal/t3bump/":             "CI tool that opens the T3 pin pull request",
	"cmd/rubi-gateway/":            "the hosted gateway's own command line, run by the host operator",
	"cmd/rubi-sandbox/":            "the maintainer sandbox, not shipped to people using Rubi",
	"internal/sandbox/":            "the maintainer sandbox, not shipped to people using Rubi",
	"cmd/rubi-announce/":           "release tool run by the maintainer",
	"cmd/rubi/usage.go":            "rubi's --help, read by the Bot that runs Rubi on its machine, not by the user",
	"skills/":                      "Bot skills (also served as rubi-panel.com/install.md): written for the Bot, which runs the commands while the user only confirms",
	"internal/mcpserver/":          "the MCP server speaks only to the Bot: its tool descriptions and results are instructions for the Bot to run",
	"internal/plugins/plugintest/": "test helper plugins, never shipped",
	"internal/e2e/":                "test support",
	"sdk/skillscan/":               "the skill scanner's own rules: it names sudo and friends to flag them",
	"internal/home/localsend.go":   "claude-send is run by a Claude session, never typed by a person: its output goes to a program",
}

type textPattern struct {
	id string
	re *regexp.Regexp
}

// patterns builds the list of what must not appear. The subcommand names are read from the two programs.
func patterns(t *testing.T) []textPattern {
	rubi := cmdNames(t, "cmd/rubi/main.go")
	home := cmdNames(t, "cmd/rubi-home/main.go")
	// "rubi" in lower case followed by one of the program's subcommands; a capital R is the product name.
	// A program's own diagnostics (usage lines, "rubi-home claude-send: ..." prefixes, "rubi daemon is already
	// running") and a command named in quotes as something a Bot must never run are statements, not instructions.
	pr := regexp.MustCompile("(?:^|[^\"\\w-])rubi\\s+(" + strings.Join(rubi, "|") + ")\\b(?:[^:]|$)|^usage: rubi ")
	ph := regexp.MustCompile("(?:^|[^\"\\w])rubi-home\\b(\\s+(" + strings.Join(home, "|") + ")\\b(?:[^:]|$)|\\s+--)")
	return []textPattern{
		{"sudo", regexp.MustCompile(`(?i)\bsudo\b`)},
		{"run-a-command", regexp.MustCompile("(?i)\\brun\\s+(`[^`]+`|the following command|this command|these commands)")},
		{"terminal", regexp.MustCompile(`(?i)\b(terminal|command prompt|powershell)\b`)},
		{"openssl", regexp.MustCompile(`(?i)\bopenssl\b`)},
		{"shell-prompt", regexp.MustCompile(`(?m)^\s*\$ \S`)},
		{"pipe-to-shell", regexp.MustCompile(`\|\s*(ba|z)?sh\b`)},
		{"curl", regexp.MustCompile(`\bcurl\s+-`)},
		{"package-manager", regexp.MustCompile(`\b(brew|apt|apt-get|npm|go)\s+install\b`)},
		{"service-manager", regexp.MustCompile(`\b(launchctl|systemctl|chmod|chown)\s`)},
		{"rubi-subcommand", pr},
		{"rubi-home-subcommand", ph},
	}
}

// cmdNames returns the subcommand names a program's main.go switches on.
func cmdNames(t *testing.T, rel string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\tcase ((?:"[a-z][a-z-]*"(?:, )?)+):`).FindAllStringSubmatch(string(b), -1) {
		for _, w := range regexp.MustCompile(`"([a-z][a-z-]*)"`).FindAllStringSubmatch(m[1], -1) {
			seen[w[1]] = true
		}
	}
	if len(seen) < 3 {
		t.Fatalf("found no subcommands in %s: the test would see nothing", rel)
	}
	var out []string
	for w := range seen {
		out = append(out, regexp.QuoteMeta(w))
	}
	sort.Strings(out)
	return out
}

var statement = regexp.MustCompile(`^rubi(-home)? [a-z-]+ (is|was|stopped|started)\b`)

type violation struct {
	file string
	text string
	ids  []string
}

// scan returns the violation in a text, or nil. A matching sentence that is handed to the Bot is not one.
func scan(pats []textPattern, file, text string) *violation {
	if strings.HasPrefix(text, "usage:") || statement.MatchString(text) { // diagnostics of a program go to whoever called it
		return nil
	}
	whole := matched(pats, text)
	if len(whole) == 0 {
		return nil
	}
	var ids []string
	for _, sen := range sentences(text) {
		if m := matched(pats, sen); len(m) > 0 && !botAddressed(sen, m) {
			ids = append(ids, m...)
		}
	}
	if len(ids) == 0 {
		// no sentence is a violation by itself: either all are for the Bot, or the pattern needed more than one sentence
		for _, sen := range sentences(text) {
			if m := matched(pats, sen); len(m) > 0 {
				return nil
			}
		}
		ids = whole
	}
	return &violation{file: file, text: text, ids: ids}
}

func matched(pats []textPattern, text string) []string {
	var ids []string
	for _, p := range pats {
		if p.re.MatchString(text) {
			ids = append(ids, p.id)
		}
	}
	return ids
}

// sentences splits a text at a line break or at a full stop, "!" or "?" followed by white space (a URL or a file name
// has no space after its dots, so a command stays whole).
func sentences(text string) []string {
	var out []string
	for _, ln := range strings.Split(text, "\n") {
		for _, p := range regexp.MustCompile(`[.!?]\s+`).Split(ln, -1) {
			if strings.TrimSpace(p) != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// The sentence opens by handing the rest to the Bot. It is anchored at the start of the sentence (only quotes,
// bullets and emphasis may come before), so "Tell your Bot" in the middle of a paragraph covers nothing.
var botHandoff = regexp.MustCompile(`(?i)^\W*(?:(?:tell|ask) your (?:grok )?bot(?::|\s+to\b)|send this to your (?:grok )?bot:|your (?:grok )?bot (?:runs|will run)\b)`)

// Words that show the sentence still asks something of the person: "you", typing, pasting, "yourself".
var userDoes = regexp.MustCompile(`(?i)\b(you|yourself|type|paste|copy)\b`)

// botAddressed says whether a sentence that holds a command is handed to the Bot (project rule 2: the Bot runs the
// command on the user's computer, the user only confirms). It is not when the sentence names a terminal or a prompt
// (those are the user's), or when anything after the hand-off asks the user to do something.
func botAddressed(sentence string, ids []string) bool {
	for _, id := range ids {
		if id == "terminal" || id == "shell-prompt" {
			return false
		}
	}
	loc := botHandoff.FindStringIndex(sentence)
	return loc != nil && !userDoes.MatchString(sentence[loc[1]:])
}

func exempt(rel string) bool {
	for p := range userTextExempt {
		if strings.HasPrefix(rel, p) {
			return true
		}
	}
	return false
}

// goTexts calls f with every string literal of a Go file that could be text for a person: not an import path or a
// struct tag, not a lone word with no space (a program name or a flag handed to exec), not an argument of
// exec.Command or regexp.MustCompile.
func goTexts(file *ast.File, f func(text string)) {
	skip := map[ast.Node]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ImportSpec:
			skip[x.Path] = true
		case *ast.Field:
			if x.Tag != nil {
				skip[x.Tag] = true
			}
		case *ast.CallExpr:
			if se, ok := x.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := se.X.(*ast.Ident); ok && (pkg.Name == "exec" || pkg.Name == "regexp") {
					for _, a := range x.Args {
						skip[a] = true
					}
				}
			}
		}
		if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING && !skip[bl] {
			if v, err := strconv.Unquote(bl.Value); err == nil && strings.ContainsAny(v, " \t\n") {
				f(v)
			}
		}
		return true
	})
}

var jsString = regexp.MustCompile("(?s)\"(?:[^\"\\\\\\n]|\\\\.)*\"|'(?:[^'\\\\\\n]|\\\\.)*'|`(?:[^`\\\\]|\\\\.)*`")

// jsTexts calls f with the contents of every string and template literal of a script (comments are dropped first).
func jsTexts(src string, f func(text string)) {
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, " ")
	var lines []string
	for _, ln := range strings.Split(src, "\n") {
		// a // comment that starts the line or follows whitespace; URLs (https://) have a colon before the slashes
		if i := regexp.MustCompile(`(^|\s)//`).FindStringIndex(ln); i != nil && !strings.Contains(ln[:i[0]+1], "\"") && !strings.Contains(ln[:i[0]+1], "'") && !strings.Contains(ln[:i[0]+1], "`") {
			ln = ln[:i[0]]
		}
		lines = append(lines, ln)
	}
	for _, m := range jsString.FindAllString(strings.Join(lines, "\n"), -1) {
		f(m[1 : len(m)-1])
	}
}

type allowEntry struct {
	file, frag, reason string
	used               bool
}

// readAllow reads testdata/usertext_allow.txt: "file || fragment of the text || T-nn: reason" per line.
func readAllow(t *testing.T) []*allowEntry {
	t.Helper()
	fh, err := os.Open(filepath.Join(root(t), "internal/repocheck/testdata/usertext_allow.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	var out []*allowEntry
	sc := bufio.NewScanner(fh)
	task := regexp.MustCompile(`^T-\d+L?: \S`)
	for n := 1; sc.Scan(); n++ {
		ln := strings.TrimSpace(sc.Text())
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		p := strings.Split(ln, " || ")
		if len(p) != 3 || p[1] == "" || !task.MatchString(p[2]) {
			t.Fatalf("usertext_allow.txt:%d: want `file || fragment || T-nn: reason`", n)
		}
		out = append(out, &allowEntry{file: p[0], frag: p[1], reason: p[2]})
	}
	return out
}

func TestNoTextTellsTheUserToUseATerminal(t *testing.T) {
	base := root(t)
	pats := patterns(t)
	var found []*violation
	add := func(rel, text string) {
		if v := scan(pats, rel, text); v != nil {
			found = append(found, v)
		}
	}

	// Go sources (not tests)
	fset := token.NewFileSet()
	_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(base, p)
		rel = filepath.ToSlash(rel)
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "vendor" || d.Name() == "testdata" || d.Name() == ".sandbox") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") || exempt(rel) {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			t.Errorf("%s: %v", rel, perr)
			return nil
		}
		goTexts(f, func(text string) { add(rel, text) })
		return nil
	})

	// the panel's scripts
	js, _ := filepath.Glob(filepath.Join(base, "panel/js/*.js"))
	for _, p := range js {
		b, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(base, p)
		jsTexts(string(b), func(text string) { add(filepath.ToSlash(rel), text) })
	}
	// the user docs
	md, _ := filepath.Glob(filepath.Join(base, "docs/user/*.md"))
	for _, p := range md {
		b, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(base, p)
		// one violation per line, so that an entry names the line it is about
		for _, ln := range strings.Split(string(b), "\n") {
			add(filepath.ToSlash(rel), ln)
		}
	}
	// the scan must see something, or a refactor could blind it
	if len(js) == 0 || len(md) == 0 {
		t.Fatal("no panel scripts or user docs found")
	}

	allow := readAllow(t)
	for _, v := range found {
		ok := false
		for _, a := range allow {
			if a.file == v.file && strings.Contains(v.text, a.frag) {
				a.used, ok = true, true
			}
		}
		if !ok {
			t.Errorf("%s tells the user to use a terminal or run a command (%s): %q\n\tproject rule 2: replace it with a step in the panel, or, if it is a text for maintainers, put the path in userTextExempt",
				v.file, strings.Join(v.ids, ", "), clipText(v.text))
		}
	}
	for _, a := range allow {
		if !a.used {
			t.Errorf("usertext_allow.txt lists %s %q (%s), but nothing matches it any more: remove the line", a.file, a.frag, a.reason)
		}
	}
}

// The patterns themselves: what each must catch and what it must leave alone.
func TestUserTextPatterns(t *testing.T) {
	pats := patterns(t)
	hit := func(s string) bool { return scan(pats, "x", s) != nil }
	for _, s := range []string{
		"Run `sudo rubi-home install-hook` on that computer", "open the Terminal app", "in the terminal", "openssl genpkey -algorithm ed25519",
		"$ rubi status", "run `rubi dev-panel`", "rubi-home pair", "curl -fsSL https://x | sh", "brew install foo", "sudo ls",
	} {
		if !hit(s) {
			t.Errorf("not caught: %q", s)
		}
	}
	for _, s := range []string{
		"Rubi is locked. Unlock it in the panel.", "Rubi update available", "the Rubi Home helper", "Open the link on your phone.",
		"price is $5", "your agent will run this for you", "rubi_status", "Tap Approve.",
	} {
		if hit(s) {
			t.Errorf("caught by mistake: %q", s)
		}
	}
}

// A command line in user-facing text is allowed only in a sentence handed to the Bot (project rule 2).
func TestACommandHandedToTheBotIsAllowedAndOneAddressedToTheUserIsNot(t *testing.T) {
	pats := patterns(t)
	hit := func(s string) bool { return scan(pats, "x", s) != nil }
	for _, s := range []string{
		"Tell your Bot: run `rubi status` on this computer.",
		"Tell your Grok Bot: install it with curl -fsSL https://rubi-panel.com/install-home.sh | sh",
		"Ask your Bot to run `sudo rubi-home install-hook`. macOS asks for the password in its own window.",
		"Your Bot runs ~/.rubi-home/bin/rubi-home open and sends the link.",
		"**Tell your Bot:** run `rubi-home pair`",
		"Open the panel.\nTell your Bot: run `rubi update`.",
	} {
		if hit(s) {
			t.Errorf("a command handed to the Bot was refused: %q", s)
		}
	}
	for _, s := range []string{
		"Open Terminal and type `rubi status`.",
		"In a terminal, run `rubi status`.",
		"Run `rubi status` on that computer.",
		"curl -fsSL https://rubi-panel.com/install-home.sh | sh",
		// a Bot phrase elsewhere in the paragraph does not cover a command addressed to the user
		"Tell your Bot you are ready. Then run `rubi status` yourself.",
		"Run `sudo ls`. Tell your Bot: thanks.",
		"Your Bot knows what to do.\nRun `rubi status` on that computer.",
		// a Bot phrase in the same sentence does not cover a terminal, a prompt, or something the user does
		"Tell your Bot: all is fine, but open Terminal and run `ls`.",
		"Tell your Bot to wait, then you run `rubi status`.",
		"Tell your Bot: paste curl -fsSL https://x | sh and press Return.",
		"Tell your Bot:\n$ rubi status",
		"Please tell your Bot: run `rubi status`",
		"Ask the user to run `rubi status`, tell your Bot: done",
	} {
		if !hit(s) {
			t.Errorf("a command addressed to the user was allowed: %q", s)
		}
	}
}

// Every path the scan skips must exist, or a rename would leave a stale exemption that skips whatever takes its place.
func TestExemptPathsExist(t *testing.T) {
	for p := range userTextExempt {
		if _, err := os.Stat(filepath.Join(root(t), filepath.FromSlash(p))); err != nil {
			t.Errorf("userTextExempt names %s, which does not exist: %v", p, err)
		}
	}
}

func clipText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}
