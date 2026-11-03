package repocheck

// Project rule 1 (docs/project-rules.md): Rubi is fully usable from a phone, and the design also supports desktop
// proportions. There is no headless browser in the dependencies, so this file holds what can be checked on the
// sources of the panel; scripts/panel-viewport-check.md is the manual check at 375x812 and 1280x800 that goes with it.
//
// The rules, for panel/styles.css and the panel's pages and scripts:
//
//	V1  every page has <meta name="viewport" content="width=device-width, initial-scale=1...">, and none turns
//	    zooming off (user-scalable=no, maximum-scale below 5).
//	W1  a width, min-width or flex-basis in px (rem/em count 16px, ch 8px) is at most 360 unless it sits in an
//	    @media (min-width: N) block with N at least that value. 360 is the narrowest phone Rubi supports; the
//	    panel's page has 20px of padding at each side, so the limit leaves room.
//	W2  grid-template-columns with fixed lengths adds up to at most 360 under the same condition.
//	W3  no width or min-width of 100vw (it ignores the scroll bar and overflows); use 100%.
//	N1  white-space: nowrap or pre in a rule needs overflow handling in the same rule (overflow, overflow-x,
//	    text-overflow), or the selector must be listed in nowrapOK with the reason it cannot push the page wide.
//	T1  no <table> in a page or script: a table is fixed-width by nature; use the list and card layouts.
//	J1  no script sets a width above 360px in an inline style.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const phoneWidth = 360

// nowrapOK are the selectors allowed white-space: nowrap without overflow handling in their own rule.
var nowrapOK = map[string]string{
	".tag":                 "a status label of one or two short words",
	".seg label":           "a segmented-control label is one short word; the control is the full width on a phone (.policy-row .seg)",
	".settings-nav button": "its parent .settings-nav scrolls sideways (overflow-x: auto)",
	".savebar button":      "an action label of one or two words (Save)",
	".bb-nav a":            "its parent .bb-nav scrolls sideways (overflow-x: auto); a design page, not a flow",
	".stepper em":          "below 420px only the current step shows its name",
}

type cssDecl struct{ prop, val string }

type cssRule struct {
	sel      string
	decls    []cssDecl
	minWidth int // the largest (min-width: Npx) among the @media blocks around the rule; 0 without
}

var (
	cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	mediaMin   = regexp.MustCompile(`\(\s*min-width\s*:\s*(\d+)px\s*\)`)
)

// parseCSS reads the rules of a style sheet, with the media conditions around each.
func parseCSS(src string) []cssRule {
	src = cssComment.ReplaceAllString(src, "")
	type frame struct {
		head string
		rule *cssRule
	}
	var stack []frame
	var out []cssRule
	var cur strings.Builder
	inParen := 0
	flushDecl := func() {
		s := strings.TrimSpace(cur.String())
		cur.Reset()
		if s == "" || len(stack) == 0 || stack[len(stack)-1].rule == nil {
			return
		}
		if i := strings.Index(s, ":"); i > 0 {
			r := stack[len(stack)-1].rule
			r.decls = append(r.decls, cssDecl{strings.ToLower(strings.TrimSpace(s[:i])), strings.TrimSpace(s[i+1:])})
		}
	}
	for _, c := range src {
		switch {
		case c == '(':
			inParen++
			cur.WriteRune(c)
		case c == ')':
			inParen--
			cur.WriteRune(c)
		case c == '{' && inParen == 0:
			head := strings.TrimSpace(cur.String())
			cur.Reset()
			f := frame{head: head}
			if !strings.HasPrefix(head, "@") {
				f.rule = &cssRule{sel: head}
				for _, o := range stack {
					if m := mediaMin.FindStringSubmatch(o.head); m != nil {
						if n, _ := strconv.Atoi(m[1]); n > f.rule.minWidth {
							f.rule.minWidth = n
						}
					}
				}
			}
			stack = append(stack, f)
		case c == '}' && inParen == 0:
			flushDecl()
			if len(stack) > 0 {
				if r := stack[len(stack)-1].rule; r != nil {
					out = append(out, *r)
				}
				stack = stack[:len(stack)-1]
			}
		case c == ';' && inParen == 0:
			flushDecl()
		default:
			cur.WriteRune(c)
		}
	}
	return out
}

var lengthRE = regexp.MustCompile(`(-?\d*\.?\d+)(px|rem|em|ch|vw)\b`)

// px converts a CSS length to pixels (vw is reported separately).
func px(num, unit string) float64 {
	n, _ := strconv.ParseFloat(num, 64)
	switch unit {
	case "rem", "em":
		return n * 16
	case "ch":
		return n * 8
	}
	return n
}

// checkCSS returns the broken rules of a style sheet.
func checkCSS(src string) []string {
	var bad []string
	for _, r := range parseCSS(src) {
		limit := float64(phoneWidth)
		if float64(r.minWidth) > limit {
			limit = float64(r.minWidth)
		}
		hasOverflow := false
		for _, d := range r.decls {
			if d.prop == "overflow" || d.prop == "overflow-x" || d.prop == "text-overflow" {
				hasOverflow = d.val != "visible"
			}
		}
		for _, d := range r.decls {
			switch d.prop {
			case "width", "min-width", "flex-basis":
				if strings.Contains(d.val, "100vw") && d.prop != "flex-basis" {
					bad = append(bad, r.sel+": "+d.prop+": "+d.val+" (W3: 100vw ignores the scroll bar; use 100%)")
				}
				if strings.Contains(d.val, "calc(") || strings.Contains(d.val, "min(") || strings.Contains(d.val, "clamp(") {
					continue // the value adapts to its container
				}
				for _, m := range lengthRE.FindAllStringSubmatch(d.val, -1) {
					if m[2] == "vw" {
						continue
					}
					if v := px(m[1], m[2]); v > limit {
						bad = append(bad, r.sel+": "+d.prop+": "+d.val+" (W1: over "+strconv.Itoa(int(limit))+"px outside a wide-screen @media)")
					}
				}
			case "grid-template-columns":
				if strings.Contains(d.val, "fr") || strings.Contains(d.val, "%") || strings.Contains(d.val, "auto") || strings.Contains(d.val, "minmax") ||
					strings.Contains(d.val, "calc(") || strings.Contains(d.val, "repeat(") {
					continue // flexible tracks shrink
				}
				sum := 0.0
				for _, m := range lengthRE.FindAllStringSubmatch(d.val, -1) {
					sum += px(m[1], m[2])
				}
				if sum > limit {
					bad = append(bad, r.sel+": grid-template-columns: "+d.val+" (W2: fixed tracks add up to "+strconv.Itoa(int(sum))+"px)")
				}
			case "white-space":
				v := strings.TrimSpace(strings.TrimSuffix(d.val, "!important"))
				if (v == "nowrap" || v == "pre") && !hasOverflow && nowrapOK[r.sel] == "" {
					bad = append(bad, r.sel+": white-space: "+v+" without overflow handling (N1)")
				}
			}
		}
	}
	return bad
}

func TestPanelStylesFitAPhone(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), "panel/styles.css"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(parseCSS(string(b))); n < 200 {
		t.Fatalf("the parser found %d rules in panel/styles.css; it no longer reads the file", n)
	}
	for _, m := range checkCSS(string(b)) {
		t.Errorf("panel/styles.css %s\n\tproject rule 1: the panel must fit 360px wide; see the rules at the top of panelcss_test.go", m)
	}
	// every entry of nowrapOK is still needed
	rules := parseCSS(string(b))
	for sel, why := range nowrapOK {
		found := false
		for _, r := range rules {
			if r.sel == sel {
				for _, d := range r.decls {
					found = found || (d.prop == "white-space" && (d.val == "nowrap" || d.val == "pre"))
				}
			}
		}
		if !found {
			t.Errorf("nowrapOK lists %q (%s), but no rule sets white-space: nowrap there any more: remove it", sel, why)
		}
	}
}

// The checker catches what it says it catches.
func TestPanelCSSCheckerHasTeeth(t *testing.T) {
	cases := []struct {
		css  string
		want int
	}{
		{`.a { width: 420px }`, 1},
		{`.a { min-width: 30rem }`, 1},
		{`.a { width: 360px; min-width: 240px }`, 0},
		{`@media (min-width: 1000px) { .a { width: 420px } }`, 0},
		{`@media (min-width: 600px) { .a { width: 700px } }`, 1},
		{`@media (max-width: 899px) { .a { width: 500px } }`, 1},
		{`.a { width: 100vw }`, 1},
		{`.a { width: min(100%, 500px) }`, 0},
		{`.a { white-space: nowrap }`, 1},
		{`.a { white-space: nowrap; overflow: hidden; text-overflow: ellipsis }`, 0},
		{`.a { white-space: pre-wrap }`, 0},
		{`.a { grid-template-columns: 200px 200px }`, 1},
		{`.a { grid-template-columns: 1fr 200px }`, 0},
		{`/* .a { width: 900px } */ .b { color: red }`, 0},
	}
	for _, c := range cases {
		if got := checkCSS(c.css); len(got) != c.want {
			t.Errorf("%s: want %d findings, got %v", c.css, c.want, got)
		}
	}
}

func TestPanelPagesAreForPhones(t *testing.T) {
	base := root(t)
	viewport := regexp.MustCompile(`<meta\s+name="viewport"\s+content="([^"]*)"`)
	pages, _ := filepath.Glob(filepath.Join(base, "panel/*.html"))
	if len(pages) == 0 {
		t.Fatal("no panel pages found")
	}
	for _, p := range pages {
		b, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(base, p)
		m := viewport.FindStringSubmatch(string(b))
		if m == nil {
			t.Errorf("%s has no viewport meta tag: a phone would draw it as a shrunk desktop page (V1)", rel)
			continue
		}
		c := strings.ReplaceAll(m[1], " ", "")
		if !strings.Contains(c, "width=device-width") || !strings.Contains(c, "initial-scale=1") {
			t.Errorf("%s: viewport %q must be width=device-width, initial-scale=1 (V1)", rel, m[1])
		}
		if strings.Contains(c, "user-scalable=no") || strings.Contains(c, "user-scalable=0") || regexp.MustCompile(`maximum-scale=[0-4](\.\d+)?(,|$)`).MatchString(c) {
			t.Errorf("%s: viewport %q turns zooming off (V1)", rel, m[1])
		}
		if regexp.MustCompile(`(?i)<table[\s>]`).Match(b) {
			t.Errorf("%s has a <table> (T1)", rel)
		}
	}
}

func TestPanelScriptsKeepToAPhone(t *testing.T) {
	base := root(t)
	js, _ := filepath.Glob(filepath.Join(base, "panel/js/*.js"))
	inline := regexp.MustCompile(`(?:min-)?width\s*:\s*(\d+(?:\.\d+)?)px`)
	for _, p := range js {
		b, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(base, p)
		src := string(b)
		if strings.Contains(src, `h("table"`) || strings.Contains(src, `createElement("table")`) || strings.Contains(src, "<table") {
			t.Errorf("%s builds a table (T1)", rel)
		}
		// widths set by a script in an inline style (J1); the demo and the mascot are drawn at fixed sizes by design
		if strings.HasSuffix(rel, "mascot.js") || strings.HasSuffix(rel, "brand.js") {
			continue
		}
		for _, m := range inline.FindAllStringSubmatch(src, -1) {
			if v, _ := strconv.ParseFloat(m[1], 64); v > phoneWidth {
				t.Errorf("%s sets a width of %spx in an inline style (J1)", rel, m[1])
			}
		}
	}
}
