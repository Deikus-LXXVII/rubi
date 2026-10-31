package skillscan

import (
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ScannerVersion changes whenever a rule or a limit changes, so a stored result can be recognised as stale.
const ScannerVersion = "skillscan/1"

// Limits (design section 7).
const (
	MaxBytes       = 32 * 1024
	MaxLines       = 800
	MaxDescription = 300
	MaxExcerpt     = 80
)

// Verdict is the strictest severity found.
type Verdict string

const (
	Pass   Verdict = "pass"
	Flag   Verdict = "flag"
	Reject Verdict = "reject"
)

// Hit is one rule match. Line is 1-based; 0 means the whole file.
type Hit struct {
	ID       string  `json:"id"`
	Line     int     `json:"line"`
	Excerpt  string  `json:"excerpt"`
	Severity Verdict `json:"severity"`
}

// Result is stored with the skill.
type Result struct {
	Verdict        Verdict  `json:"verdict"`
	Rules          []Hit    `json:"rules"`
	Stripped       []string `json:"stripped"`
	Normalized     []byte   `json:"-"` // nil when the verdict is Reject
	ScannerVersion string   `json:"scanner_version"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// ruleOrder fixes the order of hits within one line.
var ruleOrder = []string{
	"fmt.utf8", "fmt.control", "fmt.size", "fmt.lines", "fmt.name", "fmt.description", "fmt.frontmatter",
	"hid.invisible", "hid.bidi", "hid.tag", "hid.ansi", "hid.comment", "hid.dataurl", "hid.hex", "hid.base64",
	"sec.anthropic", "sec.github", "sec.gitlab", "sec.slack", "sec.aws", "sec.google", "sec.npm", "sec.pem",
	"sec.bearer", "sec.assign", "sec.entropy",
	"exf.network", "exf.credential", "exf.url", "ovr.instruction", "wid.claim",
}

var ruleRank = func() map[string]int {
	m := map[string]int{}
	for i, id := range ruleOrder {
		m[id] = i
	}
	return m
}()

type scanner struct {
	hits []Hit
	seen map[string]bool
}

// add records a hit once per (rule, line). off is the byte offset of the match in the raw line (-1: none).
func (s *scanner) add(id string, line int, raw string, off int, spans []span) {
	k := id + "\x00" + itoa(line)
	if s.seen[k] {
		return
	}
	s.seen[k] = true
	sev := Flag
	if strings.HasPrefix(id, "fmt.") || strings.HasPrefix(id, "hid.") || strings.HasPrefix(id, "sec.") {
		sev = Reject
	}
	ex := ""
	if line > 0 && off >= 0 {
		ex = excerpt(raw, off, spans)
	}
	s.hits = append(s.hits, Hit{ID: id, Line: line, Excerpt: ex, Severity: sev})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Scan checks one SKILL.md. name is the skill's name (its directory), allowedHosts is the role's allow-list plus
// the base list: exact hosts, or "*.example.com" for any subdomain.
func Scan(name string, content []byte, allowedHosts []string) Result {
	s := &scanner{seen: map[string]bool{}}
	res := Result{ScannerVersion: ScannerVersion, Rules: []Hit{}, Stripped: []string{}}

	text := string(content)
	if len(content) > MaxBytes {
		s.add("fmt.size", 0, "", -1, nil)
	}
	if !utf8.Valid(content) {
		line := 1 + strings.Count(text[:firstInvalid(text)], "\n")
		s.add("fmt.utf8", line, "", -1, nil)
	}
	lines := strings.Split(text, "\n")
	n := len(lines)
	if n > 0 && lines[n-1] == "" {
		n--
	}
	if n > MaxLines {
		s.add("fmt.lines", 0, "", -1, nil)
	}
	if !nameRe.MatchString(name) {
		s.add("fmt.name", 0, "", -1, nil)
	}

	pairs, bodyStart, present, bad := parseFrontmatter(lines)
	if !present {
		s.add("fmt.frontmatter", 1, firstLine(lines), 0, nil)
	}
	for _, l := range bad {
		s.add("fmt.frontmatter", l, lines[l-1], 0, nil)
	}
	var desc string
	haveDesc := false
	for _, p := range pairs {
		switch p.key {
		case "name":
			if p.value != name {
				s.add("fmt.name", p.line, lines[p.line-1], 0, nil)
			}
		case "description":
			desc, haveDesc = p.value, true
			if strings.TrimSpace(p.value) == "" || utf8.RuneCountInString(p.value) > MaxDescription {
				s.add("fmt.description", p.line, lines[p.line-1], 0, nil)
			}
		default:
			res.Stripped = append(res.Stripped, p.key)
		}
	}
	if present && len(bad) == 0 && !haveDesc {
		s.add("fmt.description", 0, "", -1, nil)
	}

	// An oversize file is rejected anyway: only its first 32 KiB is scanned for further reasons, to bound the work.
	scanned := lines
	if len(text) > MaxBytes {
		scanned = strings.Split(text[:MaxBytes], "\n")
	}
	for i, raw := range scanned {
		scanLine(s, i+1, raw, allowedHosts)
	}

	sort.SliceStable(s.hits, func(i, j int) bool {
		a, b := s.hits[i], s.hits[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return ruleRank[a.ID] < ruleRank[b.ID]
	})
	res.Rules = append(res.Rules, s.hits...)
	res.Verdict = Pass
	for _, h := range res.Rules {
		if h.Severity == Reject {
			res.Verdict = Reject
			break
		}
		res.Verdict = Flag
	}
	if res.Verdict != Reject {
		res.Normalized = buildNormalized(name, desc, lines[bodyStart:])
	}
	return res
}

func firstInvalid(s string) int {
	for i := 0; i < len(s); {
		r, sz := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && sz == 1 {
			return i
		}
		i += sz
	}
	return len(s)
}

func firstLine(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

// ---- per-line rules ----

type secretRule struct {
	id  string
	re  *regexp.Regexp
	grp int // submatch to mask; 0 is the whole match
}

var secretRules = []secretRule{
	{"sec.anthropic", regexp.MustCompile(`sk-ant-`), 0},
	{"sec.github", regexp.MustCompile(`(?:ghp_|gho_|github_pat_)[A-Za-z0-9_]*`), 0},
	{"sec.gitlab", regexp.MustCompile(`glpat-[A-Za-z0-9_-]*`), 0},
	{"sec.slack", regexp.MustCompile(`xox[abposr]-[A-Za-z0-9-]*`), 0},
	{"sec.aws", regexp.MustCompile(`AKIA[0-9A-Z]{16}`), 0},
	{"sec.google", regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`), 0},
	{"sec.npm", regexp.MustCompile(`npm_[A-Za-z0-9]{36}`), 0},
	{"sec.pem", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`), 0},
	{"sec.bearer", regexp.MustCompile(`Bearer\s+([A-Za-z0-9._~+/=-]{20,})`), 1},
	{"sec.assign", regexp.MustCompile(`(?i)(?:password|secret|token)\s*[:=]\s*(\S{8,})`), 1},
}

var (
	hexRunRe    = regexp.MustCompile(`[0-9A-Fa-f]{200,}`)
	b64RunRe    = regexp.MustCompile(`[A-Za-z0-9+/]{200,}={0,2}`)
	entropyTok  = regexp.MustCompile(`[A-Za-z0-9+/=_]{32,}`)
	dataURLRe   = regexp.MustCompile(`(?i)\bdata:(?:[a-z0-9.+-]+/[a-z0-9.+-]+)?(?:;[^,\s]*)?,`)
	ansiRe      = regexp.MustCompile(`(?i)(?:\x1b|\\x1b|\\033|\\e|\\u001b)\[`)
	netToolRe   = regexp.MustCompile(`\b(?:curl|wget|nc|ncat|socat|scp|rsync|ssh|ftp|telnet)\b`)
	dnsToolRe   = regexp.MustCompile(`\b(?:dig|nslookup)\b`)
	dnsSubstRe  = regexp.MustCompile("\\$\\(|`")
	uploadRe    = regexp.MustCompile(`(?:^|\s)-[dTF](?:\s|=|\S)|--data[\w-]*|--upload-file|(?:^|\s)@[\w./~-]+`)
	pipeIntoRe  = regexp.MustCompile(`\|\s*(?:sudo\s+)?(?:curl|wget|nc|ncat|socat|scp|rsync|ssh|ftp|telnet)\b`)
	credentialR = regexp.MustCompile(`(?i)~/\.ssh|id_rsa|id_ed25519|\.netrc|\.git-credentials|/proc/[^/\s]*/environ|printenv|\benv\s*\||CLAUDE_CODE_OAUTH_TOKEN|ANTHROPIC_|/rubi-claude|\.rubi/|keychain`)
	urlRe       = regexp.MustCompile(`(?i)\b(?:https?|ftp|wss?)://[^\s"'<>)\]]+`)
	overrideRe  = regexp.MustCompile(`(?i)ignore\s+(?:(?:all|previous|prior)\s+)+instructions|system\s+prompt|do\s+not\s+tell\s+the\s+user|without\s+asking|bypass|disable\s+(?:hooks|approvals|sandbox)|dangerously|passkey`)
	approveRe   = regexp.MustCompile(`(?i)approve`)
	rubiRe      = regexp.MustCompile(`(?i)rubi`)
	wideningRe  = regexp.MustCompile(`(?i)allowed-tools|permissionMode|bypassPermissions|mcpServers|settings\.json|\.claude/|--plugin-dir|allow-list|agent_allowed_hosts|add\s+(?:this|the)\s+host`)
)

func scanLine(s *scanner, ln int, raw string, allowed []string) {
	spans := secretSpans(raw)

	// Format and hidden text, by character class.
	for i, r := range raw {
		switch {
		case r == utf8.RuneError:
			// reported once for the file by fmt.utf8
		case r == '\n' || r == '\t':
		case unicode.IsControl(r):
			s.add("fmt.control", ln, raw, i, spans)
		}
		switch {
		case r == 0xAD || (r >= 0x200B && r <= 0x200F) || (r >= 0x2060 && r <= 0x2064) || r == 0xFEFF:
			s.add("hid.invisible", ln, raw, i, spans)
		case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			s.add("hid.bidi", ln, raw, i, spans)
		case r >= 0xE0000 && r <= 0xE007F:
			s.add("hid.tag", ln, raw, i, spans)
		}
	}
	if loc := ansiRe.FindStringIndex(raw); loc != nil {
		s.add("hid.ansi", ln, raw, loc[0], spans)
	}
	if i := strings.Index(raw, "<!--"); i >= 0 {
		s.add("hid.comment", ln, raw, i, spans)
	}
	if loc := dataURLRe.FindStringIndex(raw); loc != nil {
		s.add("hid.dataurl", ln, raw, loc[0], spans)
	}
	if loc := hexRunRe.FindStringIndex(raw); loc != nil {
		s.add("hid.hex", ln, raw, loc[0], spans)
	}
	if loc := b64RunRe.FindStringIndex(raw); loc != nil {
		s.add("hid.base64", ln, raw, loc[0], spans)
	}

	// Secrets.
	for _, r := range secretRules {
		if m := r.re.FindStringSubmatchIndex(raw); m != nil {
			s.add(r.id, ln, raw, m[2*r.grp], spans)
		}
	}
	for _, loc := range entropyTok.FindAllStringIndex(raw, -1) {
		if entropy(raw[loc[0]:loc[1]]) > 4.0 {
			s.add("sec.entropy", ln, raw, loc[0], spans)
			break
		}
	}

	// Exfiltration shapes.
	if loc := netToolRe.FindStringIndex(raw); loc != nil && uploadRe.MatchString(raw) {
		s.add("exf.network", ln, raw, loc[0], spans)
	} else if loc := pipeIntoRe.FindStringIndex(raw); loc != nil {
		s.add("exf.network", ln, raw, loc[0], spans)
	} else if loc := dnsToolRe.FindStringIndex(raw); loc != nil && dnsSubstRe.MatchString(raw) {
		s.add("exf.network", ln, raw, loc[0], spans)
	}
	if loc := credentialR.FindStringIndex(raw); loc != nil {
		s.add("exf.credential", ln, raw, loc[0], spans)
	}
	for _, loc := range urlRe.FindAllStringIndex(raw, -1) {
		if !hostAllowed(raw[loc[0]:loc[1]], allowed) {
			s.add("exf.url", ln, raw, loc[0], spans)
			break
		}
	}

	// Instruction overrides and widening claims.
	if loc := overrideRe.FindStringIndex(raw); loc != nil {
		s.add("ovr.instruction", ln, raw, loc[0], spans)
	} else if a := approveRe.FindStringIndex(raw); a != nil && rubiRe.MatchString(raw) {
		s.add("ovr.instruction", ln, raw, a[0], spans)
	}
	if loc := wideningRe.FindStringIndex(raw); loc != nil {
		s.add("wid.claim", ln, raw, loc[0], spans)
	}
}

func entropy(s string) float64 {
	var cnt [256]int
	for i := 0; i < len(s); i++ {
		cnt[s[i]]++
	}
	var h float64
	n := float64(len(s))
	for _, c := range cnt {
		if c > 0 {
			p := float64(c) / n
			h -= p * math.Log2(p)
		}
	}
	return h
}

func hostAllowed(raw string, allowed []string) bool {
	raw = strings.TrimRight(raw, ".,;:!?")
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if h == "" {
		return false
	}
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if strings.HasPrefix(a, "*.") {
			if strings.HasSuffix(h, a[1:]) && len(h) > len(a)-1 {
				return true
			}
		} else if h == a {
			return true
		}
	}
	return false
}

// ---- excerpts ----

type span struct{ from, to int }

// secretSpans lists the byte ranges of a line that look like secrets (merged, sorted), to be masked in excerpts.
func secretSpans(raw string) []span {
	var sp []span
	for _, r := range secretRules {
		for _, m := range r.re.FindAllStringSubmatchIndex(raw, -1) {
			if m[2*r.grp] >= 0 {
				sp = append(sp, span{m[2*r.grp], m[2*r.grp+1]})
			}
		}
	}
	for _, re := range []*regexp.Regexp{hexRunRe, b64RunRe, entropyTok} {
		for _, m := range re.FindAllStringIndex(raw, -1) {
			if re != entropyTok || entropy(raw[m[0]:m[1]]) > 4.0 {
				sp = append(sp, span{m[0], m[1]})
			}
		}
	}
	sort.Slice(sp, func(i, j int) bool { return sp[i].from < sp[j].from })
	var out []span
	for _, x := range sp {
		if k := len(out) - 1; k >= 0 && x.from <= out[k].to {
			if x.to > out[k].to {
				out[k].to = x.to
			}
			continue
		}
		out = append(out, x)
	}
	return out
}

const maskSuffix = "[masked]"

// excerpt masks secrets, shows non-printing characters as <U+XXXX>, and cuts a window of at most MaxExcerpt runes
// that starts shortly before the match at byte offset off.
func excerpt(raw string, off int, spans []span) string {
	var b strings.Builder
	moff := -1
	pos := 0
	emit := func(from, to int) {
		for i := from; i < to; {
			if i == off {
				moff = b.Len()
			}
			r, sz := utf8.DecodeRuneInString(raw[i:to])
			switch {
			case r == utf8.RuneError && sz == 1:
				b.WriteString("<invalid>")
			case r == '\t':
				b.WriteByte(' ')
			case unicode.IsControl(r) || !unicode.IsPrint(r):
				b.WriteString("<U+" + upperHex(r) + ">")
			default:
				b.WriteRune(r)
			}
			i += sz
		}
	}
	for _, sp := range spans {
		emit(pos, sp.from)
		if off >= sp.from && off < sp.to {
			moff = b.Len()
		}
		keep := sp.from
		for n := 0; n < 4 && keep < sp.to; n++ {
			_, sz := utf8.DecodeRuneInString(raw[keep:sp.to])
			keep += sz
		}
		emit(sp.from, keep)
		b.WriteString(maskSuffix)
		pos = sp.to
	}
	emit(pos, len(raw))
	if moff < 0 {
		moff = 0
	}
	out := b.String()
	runes := []rune(out)
	start := utf8.RuneCountInString(out[:moff]) - 20
	if start < 0 {
		start = 0
	}
	runes = runes[start:]
	if len(runes) > MaxExcerpt {
		runes = runes[:MaxExcerpt]
	}
	return strings.TrimSpace(string(runes))
}

func upperHex(r rune) string {
	const d = "0123456789ABCDEF"
	s := ""
	for v := int(r); v > 0; v >>= 4 {
		s = string(d[v&15]) + s
	}
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}
