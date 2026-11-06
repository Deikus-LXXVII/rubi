package repocheck

// Project rule 3 (docs/project-rules.md): uncompromising security and privacy of user data. This is the static half of
// "secrets never reach logs or the audit log"; the dynamic half is internal/core/secretlog_test.go and
// internal/audit/audit_test.go, which push known secret values through the real paths and search every output.
//
// Here: no call that writes a log line (log.Print*, a Logf, t.Log is a test) or an audit record may be handed a
// value whose expression names secret material: secret, password, passphrase, DEK, private key, bearer, api key, a
// webhook key (Agent.Key / .Key of a hook), a pairing code, a token. Naming it is not logging it, so a call may be
// listed in secretNameOK with the reason it logs something else (a count, a fingerprint, a length).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var secretish = regexp.MustCompile(`(?i)(secret|passw|passphrase|\bdek\b|privkey|privatekey|private_key|apikey|api_key|bearer|\.key\b|paircode|pairingcode|\.token\b|\btoken\b|credential|\bwraps?\b|seed)`)

// logCall reports whether a call writes to a log or the audit log.
func logCall(call *ast.CallExpr) bool {
	se, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		if id, ok := call.Fun.(*ast.Ident); ok {
			return id.Name == "logf"
		}
		return false
	}
	name := se.Sel.Name
	if pkg, ok := se.X.(*ast.Ident); ok && pkg.Name == "log" {
		return strings.HasPrefix(name, "Print") || strings.HasPrefix(name, "Fatal") || strings.HasPrefix(name, "Panic")
	}
	return name == "Logf" || name == "logf" || name == "Record" || name == "logLine"
}

// secretNameOK are the calls that name secret material without writing it, as "file: callee: first words of the text".
var secretNameOK = map[string]string{
	"internal/core/core.go: credentialID": "a passkey's public credential id, not a secret",
}

func TestNoSecretIsHandedToALogOrTheAudit(t *testing.T) {
	base := root(t)
	fset := token.NewFileSet()
	var hits []string
	used := map[string]bool{}
	walk(t, func(rel string, b []byte) {
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return
		}
		f, err := parser.ParseFile(fset, filepath.Join(base, rel), b, 0)
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			return
		}
		src := func(n ast.Node) string { return string(b[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset]) }
		check := func(call *ast.CallExpr, v ast.Expr) {
			text := src(v)
			if !secretish.MatchString(text) {
				return
			}
			key := rel + ": " + text
			if secretNameOK[key] != "" {
				used[key] = true
				return
			}
			hits = append(hits, key+" (line "+strconv.Itoa(fset.Position(call.Pos()).Line)+")")
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !logCall(call) {
				return true
			}
			for _, a := range call.Args {
				switch x := a.(type) {
				case *ast.BasicLit: // the words of the format string
				case *ast.CompositeLit: // an audit record: its keys are words, what they are set to is the value
					for _, e := range x.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok {
							check(call, kv.Value)
						}
					}
				default:
					check(call, a)
				}
			}
			return true
		})
	})
	sort.Strings(hits)
	for _, h := range hits {
		t.Errorf("a log or audit call is handed a value that names secret material: %s\n\tproject rule 3: log what happened, never the secret; if it is a count or a fingerprint, list it in secretNameOK with the reason", h)
	}
	for k, why := range secretNameOK {
		if !used[k] {
			t.Errorf("secretNameOK lists %q (%s), but no such call exists any more: remove it", k, why)
		}
	}
}
