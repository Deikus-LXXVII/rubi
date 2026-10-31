package sshtarget

import (
	"strings"
	"unicode"
)

// shellSafe are the characters a command may carry after a pattern's prefix: no shell operators, quotes,
// substitutions, redirections or line breaks, so a prefix can't be extended into a second command.
func shellSafe(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune(" _./:=@%+,-", r):
		default:
			return false
		}
	}
	return true
}

// plainArgs says whether the arguments hold no option (a word starting with "-") and no "name=value".
func plainArgs(s string) bool {
	if strings.Contains(s, "=") {
		return false
	}
	for _, w := range strings.Fields(s) {
		if strings.HasPrefix(w, "-") {
			return false
		}
	}
	return true
}

// Displayable says whether a command can be shown to the user as it is: no control or format characters (line
// breaks, escapes, bidirectional overrides, zero-width characters) that could make the text differ from what runs.
func Displayable(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || r == 0xFFFD
	}) < 0
}

// OptionPattern says whether an allowed-command pattern is the "prefix **" form: any arguments, options included.
func OptionPattern(p string) bool { return strings.HasSuffix(p, " **") || p == "**" }

// Allows says whether the target lets this command run. With no allowed commands any (non-empty) command
// runs. Patterns:
//
//   - an exact command ("uptime"): the command must equal it;
//   - "prefix *": the command is the prefix alone or the prefix, a space, and arguments of plain characters
//     (letters, digits and " _./:=@%+,-") that hold no option (no word starting with "-") and no "="; "ls*"
//     reads as "ls *", so it does not allow "lsof";
//   - "prefix **": as "prefix *", but options and "name=value" arguments are allowed too (the panel warns).
//
// A prefix pattern is not a safety boundary for a program with options that run code or open files ("git -c
// core.sshCommand=...", "tar --checkpoint-action=exec=...", "rsync -e ..."): allow such programs only with exact
// commands, never with a "**" prefix and think twice about a "*" one.
func (t Target) Allows(cmd string) bool {
	if cmd == "" || len(cmd) > MaxCommandLen || strings.ContainsAny(cmd, "\x00\r\n") {
		return false
	}
	if len(t.AllowedCommands) == 0 {
		return true
	}
	for _, p := range t.AllowedCommands {
		pre, ok := strings.CutSuffix(p, "*")
		if !ok {
			if cmd == p {
				return true
			}
			continue
		}
		opts := false
		if q, ok := strings.CutSuffix(pre, "*"); ok {
			pre, opts = q, true
		}
		pre = strings.TrimRight(pre, " ")
		rest := cmd
		if pre != "" {
			if cmd == pre {
				return true
			}
			var ok bool
			if rest, ok = strings.CutPrefix(cmd, pre+" "); !ok {
				continue
			}
		}
		if shellSafe(rest) && (opts || plainArgs(rest)) {
			return true
		}
	}
	return false
}
