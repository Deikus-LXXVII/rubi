package setup

import "strings"

// asString quotes text as an AppleScript string literal.
func asString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// adminCommand is the macOS administrator run of a shell command line: osascript's "do shell script ... with administrator
// privileges". The line is built by installCommandAs from a constant script and quoted words (shellQuote), and the
// AppleScript string literal quotes it again.
func adminCommand(sh string) []string {
	return []string{"/usr/bin/osascript", "-e", "do shell script " + asString(sh) + " with administrator privileges"}
}
