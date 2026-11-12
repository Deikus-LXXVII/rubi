package repocheck

// A plugin that declares account secrets (or fields) must have a panel path to the
// account form, even when it has its own page (panel === "view"). setupScreen used
// to send every view-panel plugin to pluginViewScreen, and the settings Connect
// button called setupScreen, so T3 Agents' github_token could not be entered.

import (
	"regexp"
	"strings"
	"testing"
)

func TestViewPanelPluginSecretsHaveAReachableForm(t *testing.T) {
	js := readRepo(t, "panel/js/app.js")

	account := jsFunction(t, js, "accountSetupScreen")
	if account == "" {
		t.Fatal("panel/js/app.js has no accountSetupScreen: a view-panel plugin that declares secrets has no form that calls integration.setup")
	}
	if !strings.Contains(account, `"integration.setup"`) {
		t.Error("accountSetupScreen does not call integration.setup")
	}
	if !strings.Contains(account, "confirmChange") {
		t.Error("accountSetupScreen does not go through the passkey confirmChange flow")
	}
	if !strings.Contains(account, "keeps its settings") {
		t.Error("updating an existing account's secret must keep its settings; the form no longer says so")
	}
	if regexp.MustCompile(`entry\.panel\s*===\s*"view"`).MatchString(account) {
		t.Error("accountSetupScreen redirects view-panel plugins away from the secrets form")
	}
	if !strings.Contains(account, "secrets") {
		t.Error("accountSetupScreen does not collect secrets")
	}

	settings := jsFunction(t, js, "settingsScreen")
	if !regexp.MustCompile(`p\.connected \? "Add account" : "Connect"`).MatchString(settings) {
		t.Fatal("the settings plugin card no longer has a Connect/Add account button")
	}
	if !regexp.MustCompile(`accountSetupScreen\(\s*ctx,\s*p\.id`).MatchString(settings) {
		t.Error("settings Connect/Add account does not open accountSetupScreen; a view-panel plugin with secrets would never reach the form")
	}
	if regexp.MustCompile(`onclick:\s*\(\)\s*=>\s*setupScreen\(\s*ctx,\s*p\.id`).MatchString(settings) {
		t.Error("settings Connect still calls setupScreen, which sends view-panel plugins to their own page")
	}

	view := jsFunction(t, js, "pluginViewScreen")
	if !strings.Contains(view, "accountSetupScreen") {
		t.Error("pluginViewScreen has no path to accountSetupScreen; a setup: link for a view-panel plugin would not reach the token form")
	}
}

// jsFunction is the source of a function (or async function) named name in js, including nested
// functions, found by matching braces. Strings and comments are skipped when counting braces.
// Default arguments such as `viewArgs = {}` are not mistaken for the body.
func jsFunction(t *testing.T, js, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?:async\s+)?function\s+` + regexp.QuoteMeta(name) + `\s*\(`)
	loc := re.FindStringIndex(js)
	if loc == nil {
		return ""
	}
	openParen := loc[1] - 1
	closeParen := jsMatchPair(js, openParen, '(', ')')
	if closeParen < 0 {
		t.Fatalf("function %s: unmatched parameter list", name)
	}
	i := strings.IndexByte(js[closeParen+1:], '{')
	if i < 0 {
		t.Fatalf("function %s has no body", name)
	}
	start := closeParen + 1 + i
	end := jsMatchPair(js, start, '{', '}')
	if end < 0 {
		t.Fatalf("function %s: unmatched brace", name)
	}
	return js[start : end+1]
}

func jsMatchBrace(s string, open int) int { return jsMatchPair(s, open, '{', '}') }

func jsMatchPair(s string, open int, openCh, closeCh byte) int {
	depth := 0
	for i := open; i < len(s); {
		c := s[i]
		switch c {
		case '/':
			if i+1 < len(s) && s[i+1] == '/' {
				if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
					i += j + 1
					continue
				}
				return -1
			}
			if i+1 < len(s) && s[i+1] == '*' {
				if j := strings.Index(s[i+2:], "*/"); j >= 0 {
					i += 2 + j + 2
					continue
				}
				return -1
			}
		case '\'', '"', '`':
			i = jsSkipString(s, i)
			continue
		default:
			if c == openCh {
				depth++
			} else if c == closeCh {
				depth--
				if depth == 0 {
					return i
				}
			}
		}
		i++
	}
	return -1
}

func jsSkipString(s string, i int) int {
	q := s[i]
	i++
	for i < len(s) {
		if s[i] == '\\' {
			i += 2
			continue
		}
		if s[i] == q {
			return i + 1
		}
		if q == '`' && s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			j := jsMatchBrace(s, i+1)
			if j < 0 {
				return len(s)
			}
			i = j + 1
			continue
		}
		i++
	}
	return len(s)
}
