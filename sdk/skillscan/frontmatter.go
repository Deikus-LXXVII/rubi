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
		if !ok || !fmKeyRe.MatchString(k) || (v != "" && v[0] != ' ' && v[0] != '\t') || seen[k] {
			bad = append(bad, i+1)
			continue
		}
		seen[k] = true
		pairs = append(pairs, kv{key: k, value: unquote(strings.TrimSpace(v)), line: i + 1})
	}
	return pairs, end + 1, true, bad
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

// buildNormalized rebuilds SKILL.md with a frontmatter of name and description only.
func buildNormalized(name, desc string, body []string) []byte {
	d, _ := jsonString(desc)
	var b strings.Builder
	b.WriteString("---\nname: " + name + "\ndescription: " + d + "\n---\n")
	b.WriteString(strings.Join(body, "\n"))
	s := b.String()
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return []byte(s)
}

func jsonString(s string) (string, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	err := enc.Encode(s)
	return strings.TrimSuffix(sb.String(), "\n"), err
}
