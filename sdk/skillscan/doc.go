// Package skillscan is the deterministic scanner for agent skills (a SKILL.md file).
//
// It lives in the core module's sdk/ directory so that a plugin in a separate repository can import it, like
// sdk/rubiplugin. It has no network access, no model and no state: the same input gives the same Result, with the
// hits in a stable order (by line, then by rule order).
//
// Scan applies three kinds of checks and returns the strictest verdict:
//
//   - reject: never activated, no override exists. Format limits (UTF-8, control characters, 32 KiB, 800 lines,
//     name and description, a simple "key: value" frontmatter), hidden text (zero-width and bidi characters, Unicode
//     tag characters, ANSI escapes, HTML comments, data: URLs, base64 or hex runs of 200+ characters) and secrets
//     (known token prefixes, PEM private-key headers, bearer tokens, password/secret/token assignments, and
//     high-entropy strings).
//   - flag: needs a person to acknowledge each flag. Exfiltration shapes (network tools with upload options,
//     credential paths, URLs whose host is not allowed), instruction overrides and widening claims.
//   - strip: frontmatter keys other than name and description are removed from Result.Normalized and listed in
//     Result.Stripped. A skill never carries grants.
//
// Excerpts in the hits are at most 80 characters, show non-printing characters as <U+XXXX>, and never contain a full
// secret: anything shaped like a secret is cut to its first four characters followed by [masked].
//
// Honest limit: paraphrased instructions get past any keyword scan. The protections that matter are structural (no
// grants from skills, every call through an approval, egress only through the allow-list); see
// docs/design/agent-roles.md section 7.
//
// Rule IDs (the order below is the order of hits within a line):
//
//	reject: fmt.utf8 fmt.control fmt.size fmt.lines fmt.name fmt.description fmt.frontmatter
//	        hid.invisible hid.bidi hid.tag hid.ansi hid.comment hid.dataurl hid.hex hid.base64
//	        sec.anthropic sec.github sec.gitlab sec.slack sec.aws sec.google sec.npm sec.pem
//	        sec.bearer sec.assign sec.entropy
//	flag:   exf.network exf.credential exf.url ovr.instruction wid.claim
//
// Example:
//
//	res := skillscan.Scan("my-skill", content, []string{"example.com", "*.example.org"})
//	switch res.Verdict {
//	case skillscan.Reject: // never activate
//	case skillscan.Flag:   // hold until accepted
//	case skillscan.Pass:   // res.Normalized is what gets materialized
//	}
package skillscan
