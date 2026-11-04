package home

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The Windows form of the plan. A Unix plan is a shell script that root runs; Windows has no /bin/sh, no chown and no
// mktemp, so the plan is data: a JSON document that says what to copy where, with the hash of every file. The elevated
// side (elevate_uac.go) is a constant PowerShell script that reads the document, but only after checking that the file it
// reads has the SHA-256 that was on its command line, and that then does what the document says: reads the binary once,
// checks its hash, writes it next to its target, sets the owner and the access list (Administrators and SYSTEM full
// control, Users read and execute, nothing inherited) and moves it into place. Nothing the user can write is used after
// it was checked.

// winPlans is true where plans are Windows plans. A variable, so that the other systems' tests can build one.
var winPlans = runtime.GOOS == "windows"

type winBinary struct {
	Src    string `json:"src"`
	Dst    string `json:"dst"`
	SHA256 string `json:"sha256"`
}

type winPluginFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Data   string `json:"data"` // base64
}

type winPlugin struct {
	Dst   string          `json:"dst"`
	Files []winPluginFile `json:"files"`
}

// windowsInstallScript is the binary part of a Windows plan.
func windowsInstallScript(src, dst, want string) string {
	b, _ := json.Marshal(winBinary{Src: src, Dst: dst, SHA256: want})
	return string(b)
}

// windowsPluginScript is the plugin part of a Windows plan.
func windowsPluginScript(dst string, files []PluginFile) string {
	w := winPlugin{Dst: dst}
	for _, f := range files {
		w.Files = append(w.Files, winPluginFile{Path: f.Path, SHA256: f.SHA256, Data: base64.StdEncoding.EncodeToString(f.Data)})
	}
	b, _ := json.Marshal(w)
	return string(b)
}

// windowsFullPlan is the document the elevated side reads: both parts, in a fixed shape.
func windowsFullPlan(binary, plugin string) string {
	if plugin == "" {
		plugin = "null"
	}
	return `{"v":1,"binary":` + binary + `,"plugin":` + plugin + `}`
}

var winAbsPath = regexp.MustCompile(`^[A-Za-z]:\\`)

// unsafeWinPath is unsafePath for Windows: a drive path, with none of the characters that a document, a prompt or a
// script would have to treat specially, and no stream or device syntax.
func unsafeWinPath(p string) string {
	if !winAbsPath.MatchString(p) {
		return "it isn't an absolute path on a drive"
	}
	if !utf8.ValidString(p) {
		return "it isn't valid UTF-8"
	}
	for i, r := range p {
		switch {
		case r == ':' && i != 1:
			return "it contains a colon (a stream or a device name)"
		case strings.ContainsRune(`"'`+"`"+`$%<>|?*^;&`, r), unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			return fmt.Sprintf("it contains %q, which can't be passed safely to the administrator step", r)
		}
	}
	return ""
}
