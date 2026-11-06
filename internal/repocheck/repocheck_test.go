// Package repocheck holds tests that read the repository's own sources. They keep out of the code what was taken out on
// purpose: the stored subscription token, the auth gateway that added it to requests, and any way for Rubi Home to open
// the credential that claude keeps in the sign-in folder.
package repocheck

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func root(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// walk calls f for every source file of the repository (not the docs, the changelog, the vendored or built folders).
func walk(t *testing.T, f func(rel string, b []byte)) {
	t.Helper()
	base := root(t)
	skipDir := map[string]bool{".git": true, "node_modules": true, "dist": true, "dist-test": true, "docs": true, "vendor": true}
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, p)
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "CHANGELOG.md" || strings.HasSuffix(d.Name(), ".md") || strings.HasSuffix(d.Name(), ".lock") || strings.HasSuffix(d.Name(), "package-lock.json") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil || bytes.IndexByte(b, 0) >= 0 { // unreadable or binary
			return nil
		}
		f(filepath.ToSlash(rel), b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// retired are the names of what was removed. A file that must still mention one (to prove it is gone, or to reserve a
// number) is listed with the reason; the docs and the changelog are not read at all.
var retired = []string{
	"oauth-token", "setup-token", "CLAUDE_CODE_OAUTH_TOKEN", "rubi-claude-token", "RUBI_SETUP_TOKEN_CMD",
	"SvcGateway", "SVC_GATEWAY", "StartGateway", "GatewayConfig", "PipeGatewayAddr", "gatewaySeam", "SetTestGatewayUpstream", "TokenFileOf",
	"gatewaytest",
}

var allowed = map[string]struct {
	names  []string
	reason string
}{
	"internal/repocheck/repocheck_test.go":        {retired, "this test names what it looks for"},
	"internal/home/agent/netpipe/mux.go":          {[]string{"SvcGateway"}, "service 2 is reserved and refused: the number is never reused"},
	"internal/home/agent/netpipe/doc.go":          {[]string{"SvcGateway"}, "the package documentation says service 2 is reserved"},
	"internal/home/agent/auth_test.go":            {[]string{"SvcGateway"}, "proves the sign-in container offers no service 2"},
	"internal/home/agent/pipe_test.go":            {[]string{"SvcGateway"}, "proves a netless VM's pipe offers no service 2"},
	"internal/home/agent/manager_netless_test.go": {[]string{"SvcGateway"}, "proves a runner VM's pipe offers no service 2"},
	"internal/home/agent/t3vm/server_test.go":     {[]string{"CLAUDE_CODE_OAUTH_TOKEN"}, "T3's environment may hold the variable; the test proves it never reaches the VM"},
	"internal/home/agent/t3vm/env_test.go":        {[]string{"CLAUDE_CODE_OAUTH_TOKEN"}, "T3's environment may hold the variable; the test proves it is dropped"},
	"sdk/skillscan/scan.go":                       {[]string{"CLAUDE_CODE_OAUTH_TOKEN"}, "the skill scanner flags a skill that names the variable"},
	"runner/embed_test.go":                        {[]string{"CLAUDE_CODE_OAUTH_TOKEN"}, "proves the image bakes no credential in"},
	"runner/test/netpipe.test.ts":                 {[]string{"SVC_GATEWAY"}, "proves the forwarder has no service 2"},
	"internal/home/agent/retired_test.go":         {nil, "(no retired name; listed so a future one is a conscious choice)"},
}

// Nothing of the removed token and gateway design is in the sources, except where a file is allowed to say so.
func TestRetiredTokenAndGatewayNamesStayOut(t *testing.T) {
	found := map[string]map[string]bool{} // file -> names
	walk(t, func(rel string, b []byte) {
		for _, n := range retired {
			if bytes.Contains(b, []byte(n)) {
				if found[rel] == nil {
					found[rel] = map[string]bool{}
				}
				found[rel][n] = true
			}
		}
	})
	var files []string
	for f := range found {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		ok := map[string]bool{}
		if a, has := allowed[f]; has {
			for _, n := range a.names {
				ok[n] = true
			}
		}
		for n := range found[f] {
			if !ok[n] {
				t.Errorf("%s names %q, which was removed with the token store and the auth gateway (docs/plans/t3-integration.md S5)", f, n)
			}
		}
	}
	// An allow-list entry that no longer matches anything is stale: remove it, so the list only ever shrinks.
	for f, a := range allowed {
		for _, n := range a.names {
			if f == "internal/repocheck/repocheck_test.go" {
				continue
			}
			if !found[f][n] {
				t.Errorf("the allow-list says %s names %q (%s), but it does not", f, n, a.reason)
			}
		}
	}
}

// The removed pieces are really gone from the tree.
func TestRemovedFilesAreGone(t *testing.T) {
	base := root(t)
	for _, p := range []string{
		"internal/home/agent/gateway.go", "internal/home/agent/gateway_policy.go", "internal/home/agent/gateway_seam.go",
		"internal/home/agent/gateway_testhook.go", "runner/container/rubi-claude-token", "runner/test/token.test.ts",
		"runner/test/support/fake-setup-token.mjs", "internal/home/setup/selftest.go",
	} {
		if _, err := os.Stat(filepath.Join(base, p)); err == nil {
			t.Errorf("%s is back", p)
		}
	}
}

// credentialNames are the names through which Go code reaches the sign-in folder or the credential file in it.
var credentialNames = []string{"CredentialFile", ".credentials.json", "SignInDirOf", "SignInDirName", "signInDir", "SignInDir", "agent-signin"}

// riskyOS are the functions of package os that open, read or change a file or folder.
var riskyOS = map[string]bool{
	"Open": true, "OpenFile": true, "ReadFile": true, "ReadDir": true, "Create": true, "WriteFile": true, "Truncate": true,
	"Chmod": true, "Chown": true, "Rename": true, "Remove": true, "RemoveAll": true, "Link": true, "Symlink": true, "Chtimes": true,
}

// Rubi Home never opens, reads, writes or deletes anything in the sign-in folder through package os: the only touch is
// the Lstat of the credential file in agent.SignedInAt, and the folder is made (MkdirAll, Chmod 0700) by
// agent.EnsureSignInDir. A call into package os that names the folder or the file in its arguments is refused, anywhere
// in the code (tests may make and chmod the files they plant).
func TestHomeNeverOpensTheSignInFolderOrItsCredential(t *testing.T) {
	// the two functions that touch the folder, with what each may do
	allowedIn := map[string]map[string]bool{
		"EnsureSignInDir": {"MkdirAll": true, "Lstat": true, "Chmod": true, "EvalSymlinks": true},
		"SignedInAt":      {"Lstat": true},
	}
	touched := map[string]bool{}
	base := root(t)
	fset := token.NewFileSet()
	walk(t, func(rel string, b []byte) {
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return
		}
		f, err := parser.ParseFile(fset, filepath.Join(base, rel), b, 0)
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			return
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "os" {
					return true
				}
				var args bytes.Buffer
				for _, a := range call.Args {
					args.Write(b[fset.Position(a.Pos()).Offset:fset.Position(a.End()).Offset])
					args.WriteByte(' ')
				}
				names := false
				for _, c := range credentialNames {
					names = names || strings.Contains(args.String(), c)
				}
				if !names {
					return true
				}
				where := rel + " " + fd.Name.Name
				if !allowedIn[fd.Name.Name][sel.Sel.Name] || !strings.HasSuffix(rel, "internal/home/agent/auth.go") {
					t.Errorf("%s calls os.%s on the sign-in folder or its credential: %s", where, sel.Sel.Name, strings.TrimSpace(args.String()))
				}
				touched[fd.Name.Name+" "+sel.Sel.Name] = true
				return true
			})
		}
	})
	_ = riskyOS
	// the allowed touches really exist where the test says (a rename of the function must not blind the test)
	if !touched["SignedInAt Lstat"] {
		t.Error("agent.SignedInAt no longer stats the credential file, or the test no longer sees it")
	}
}
