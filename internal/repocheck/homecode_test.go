package repocheck

// Rubi Home's window opens the panel with its pairing code in the address fragment (#home-code=...). The panel
// has no JavaScript test harness, so the Source tests read app.js and homecode.js the way panelflows_test.go does
// (they check the shape of the code, not what it does; homecode_node_test.go runs the pure helpers under node
// when node is installed). Together they hold the promises that keep the code safe:
//
//	the handler runs before parseLink and takes the code out of the address bar before anything else uses it;
//	the kept item has an expiry that is checked on every read, and an expired one is deleted;
//	the code is deleted after a successful pair and when the box is cleared, and it is never drawn in full.

import (
	"regexp"
	"strings"
	"testing"
)

// jsFunc returns the source of "function name(...) {...}" in js (braces counted; the panel's strings and
// regexes in these functions hold no unbalanced braces).
func jsFunc(t *testing.T, js, name string) string {
	t.Helper()
	i := strings.Index(js, "function "+name+"(")
	if i < 0 {
		t.Fatalf("function %s not found in the panel", name)
	}
	open := strings.Index(js[i:], "{")
	depth := 0
	for j := i + open; j < len(js); j++ {
		switch js[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return js[i : j+1]
			}
		}
	}
	t.Fatalf("function %s is not closed", name)
	return ""
}

func before(s, a, b string) bool {
	i, j := strings.Index(s, a), strings.Index(s, b)
	return i >= 0 && j >= 0 && i < j
}

func TestHomeCodeSourceHandlerRemovesTheCodeFromTheURLFirst(t *testing.T) {
	js := panelJS(t)
	m := jsFunc(t, js, "main")
	if !before(m, `startsWith("#home-code=")`, "parseLink(location.hash)") {
		t.Error("main() must handle #home-code= before parseLink")
	}
	f := jsFunc(t, js, "homeCodeArrived")
	if !strings.Contains(f, "Copy the code from Rubi Home's window and paste it in Settings, Rubi Home.") ||
		!regexp.MustCompile(`const kept = keepHomeCode\(`).MatchString(f) {
		t.Error("the arrival page must tell the user to copy the code when nothing was stored")
	}
	if i := strings.Index(m, "waitingHomeCode()"); i < 0 || i > strings.Index(m, `startsWith("#demo")`) {
		t.Error("main() must delete an expired code before any page is chosen")
	}
	if !before(f, "history.replaceState", "keepHomeCode(") {
		t.Error("homeCodeArrived must call history.replaceState before it stores or uses the code")
	}
	if !before(f, "history.replaceState", "screen(") {
		t.Error("homeCodeArrived must clear the address bar before it draws anything")
	}
	if regexp.MustCompile(`console\.`).MatchString(f) {
		t.Error("homeCodeArrived must not log the code")
	}
	// the page it shows never holds the code: nothing after screen( names the variable
	if regexp.MustCompile(`[,(\[+]\s*code\s*[,)\]+.]`).MatchString(f[strings.Index(f, "screen("):]) {
		t.Error("the arrival page must not show the code")
	}
}

func TestHomeCodeSourceKeepsAnExpiryAndDeletes(t *testing.T) {
	js := panelJS(t)
	if k := jsFunc(t, js, "keepHomeCode"); !regexp.MustCompile(`until:\s*now\s*\+\s*HOME_CODE_MS`).MatchString(k) ||
		!strings.Contains(k, "try") || !strings.Contains(k, "catch") {
		t.Error("keepHomeCode must store {code, until: now + HOME_CODE_MS} inside try/catch")
	}
	if !regexp.MustCompile(`HOME_CODE_MS\s*=\s*15\s*\*\s*60\s*\*\s*1000`).MatchString(js) {
		t.Error("the code must live 15 minutes")
	}
	w := jsFunc(t, js, "waitingHomeCode")
	if !regexp.MustCompile(`until\)\s*>\s*now`).MatchString(w) || !strings.Contains(w, "clearHomeCode()") || !strings.Contains(w, "catch") {
		t.Error("waitingHomeCode must check the expiry, delete an expired item and survive bad storage")
	}
	d := jsFunc(t, js, "homeDeviceScreen")
	if !strings.Contains(d, "waitingHomeCode()") {
		t.Error("the add-a-home-computer screen must prefill from the waiting code")
	}
	if i := strings.Index(d, `"device.pair"`); i < 0 || !strings.Contains(d[i:], "clearHomeCode()") {
		t.Error("the code must be deleted after a successful device.pair")
	}
	if a, p := strings.Index(d, `addEventListener("input"`), strings.Index(d, `"device.pair"`); a < 0 || p < a || !strings.Contains(d[a:p], "clearHomeCode()") {
		t.Error("clearing the box must delete the waiting code")
	}
	if !regexp.MustCompile(`"device\.pair",\s*\{[^}]*pin:`).MatchString(d) {
		t.Error("the PIN must be sent with the code")
	}
	if !strings.Contains(js, "Pair the waiting Rubi Home") {
		t.Error("the Rubi Home section needs its one-tap entry")
	}
}
