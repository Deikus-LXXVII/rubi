package agenttask

import "strings"

// ReportFromText accepts only an explicit terminal report at the END of a turn.
// Ordinary replies and questions must not silently authorize publication.
func ReportFromText(text string) Summary {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	r := Summary{Work: clipReport(text, 4096)}
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "STATUS: PASS" && line != "STATUS: ISSUES" && line != "STATUS: BLOCKED" {
			continue
		}
		if i+2 >= len(lines) || !strings.HasPrefix(lines[i+1], "COMMIT: ") {
			return r
		}
		var evidence []string
		for _, line := range lines[i+2:] {
			if !strings.HasPrefix(line, "EVIDENCE: ") {
				return r
			}
			evidence = append(evidence, strings.TrimPrefix(line, "EVIDENCE: "))
		}
		r.Status = strings.TrimPrefix(line, "STATUS: ")
		r.Tests = clipReport(strings.Join(evidence, "\n"), 4096)
		return r
	}
	return r
}
func clipReport(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
