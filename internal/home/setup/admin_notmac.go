//go:build !darwin

package setup

// asString is the macOS AppleScript quoting (admin_darwin.go); there is none here.
func asString(s string) string { return s }

// adminCommand is the macOS administrator run (admin_darwin.go): Apple's container tool is installed on a Mac only, so
// there is no command to run elsewhere.
func adminCommand(string) []string { return nil }
