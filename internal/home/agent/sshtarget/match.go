package sshtarget

import "strings"

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

// Allows says whether the target lets this command run. With no allowed commands any (non-empty) command
// runs. A pattern ending in "*" is a prefix: the command starts with the text before the "*" and the rest has
// only plain characters (letters, digits and " _./:=@%+,-"). Any other pattern must equal the command.
func (t Target) Allows(cmd string) bool {
	if cmd == "" || len(cmd) > MaxCommandLen || strings.ContainsAny(cmd, "\x00\r\n") {
		return false
	}
	if len(t.AllowedCommands) == 0 {
		return true
	}
	for _, p := range t.AllowedCommands {
		if pre, ok := strings.CutSuffix(p, "*"); ok {
			if rest, ok := strings.CutPrefix(cmd, pre); ok && shellSafe(rest) {
				return true
			}
		} else if cmd == p {
			return true
		}
	}
	return false
}
