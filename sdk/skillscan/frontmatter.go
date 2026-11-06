package skillscan

import (
	"encoding/json"
	"regexp"
	"strings"
)

type kv struct {
	key, value string
	line       int
}

var fmKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// parseFrontmatter reads a leading "---" block of simple "key: value" lines. present is false when the first line
// is not "---". bodyStart is the index (0-based) of the first line after the closing "---". bad lists the 1-based
// lines that break the simple format (line 1 when the block is never closed).
func parseFrontmatter(lines []string) (pairs []kv, bodyStart int, present bool, bad []int) {
	if len(lines) == 0 || lines[0] != "---" {
		return nil, 0, false, nil
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, len(lines), true, []int{1}
	}
	seen := map[string]bool{}
	for i := 1; i < end; i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" {
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok || !fmKeyRe.MatchString(k) || (v != "" && v[0] != ' ' && v[0] != '\t') || seen[k] || hasEscape(v) {
			bad = append(bad, i+1)
			continue
		}
		seen[k] = true
		pairs = append(pairs, kv{key: k, value: unquote(strings.TrimSpace(v)), line: i + 1})
	}
	return pairs, end + 1, true, bad
}

// hasEscape says whether a double-quoted value holds a backslash: escapes would let the scanned text differ from
// the decoded text that is emitted, so none is accepted.
func hasEscape(v string) bool {
	v = strings.TrimSpace(v)
	return strings.HasPrefix(v, "\"") && strings.Contains(v, "\\")
}

func unquote(v string) string {
	if len(v) >= 2 {
		if v[0] == '"' && v[len(v)-1] == '"' {
			var s string
			if json.Unmarshal([]byte(v), &s) == nil {
				return s
			}
			return v[1 : len(v)-1]
		}
		if v[0] == '\'' && v[len(v)-1] == '\'' {
			return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
		}
	}
	return v
}

// quoteYAML writes s as a single-quoted YAML scalar: nothing in it is an escape, so what is scanned is what is read
// (and a name such as yes, null or 123 stays a string).
func quoteYAML(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// QuoteYAML is quoteYAML for the code that writes the other markdown files of a role.
func QuoteYAML(s string) string { return quoteYAML(s) }

// buildNormalized rebuilds SKILL.md with a frontmatter of name and description only, both single-quoted.
func buildNormalized(name, desc string, body []string) []byte {
	var b strings.Builder
	b.WriteString("---\nname: " + quoteYAML(name) + "\ndescription: " + quoteYAML(desc) + "\n---\n")
	b.WriteString(strings.Join(body, "\n"))
	s := b.String()
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return []byte(s)
}
