package repocheck

// Rubi Home must work on macOS, Linux and Windows (docs/plans/home-platforms.md). This test keeps new Home code
// from depending on macOS alone, and keeps the old code that does on a list that only shrinks.
//
// Two checks read the Go sources of cmd/rubi-home, internal/home/... and internal/homeproto (not the tests):
//
//  1. Mac-only things outside a darwin-only file. A file is darwin-only when its name or its build constraint
//     makes it build for macOS and for neither Linux nor Windows (peer_darwin.go, or //go:build darwin). Any
//     other file is checked for, as parsed with go/ast (no blind grep):
//     - a macOS tool run or looked up: exec.Command, exec.CommandContext or exec.LookPath with osascript,
//     launchctl, pbcopy, open, shortcuts, container, codesign, pkgutil... (a string constant of the same package
//     counts), the first word of a []string argv, or any string that is the absolute path of such a tool;
//     - macOS paths: /Library, ~/Library, "Library" as a path element, Application Support, LaunchAgents,
//     Info.plist, the .app bundle folders; a property list or an AppleScript "do shell script" in a string;
//     - kernel interfaces: a "kern." sysctl name, unix.Sysctl*, unix.LOCAL_*, unix.SOL_LOCAL, unix.Xucred;
//     - Apple's container tool as the VM: agent.AppleContainer or agent.ContainerBin used from outside the agent
//     package.
//     A runtime.GOOS test does not excuse the use: the macOS part goes in a _darwin.go file, with the same
//     functions in a _linux.go and _windows.go (or one _other.go) next to it.
//  2. Every darwin-only file has a counterpart for linux and for windows: files of the same directory that start
//     with the same name and a "_", build for that OS, and declare every function and method of the darwin file.
//
// Existing findings are listed in testdata/homeplatform_allow.txt, one per file and item:
//
//	file || item || Pn: reason
//
// Pn is the stage of docs/plans/home-platforms.md that removes it (the plan must have a heading for it). A new
// finding fails, and so does a line that matches nothing any more: the list only ever shrinks.
//
// The net is a heuristic: the plan lists every macOS dependency, this finds the ones that show as a literal. The
// backstop for code that only compiles on a Unix is the cross-build of stage P1 (GOOS=windows go build).

import (
	"bufio"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// hpDirs are the Home packages (slash paths from the repository root).
var hpDirs = []string{"cmd/rubi-home", "internal/home", "internal/homeproto"}

// hpMacTools are programs that exist on macOS only (or mean something else there).
var hpMacTools = map[string]bool{
	"osascript": true, "launchctl": true, "pbcopy": true, "pbpaste": true, "open": true, "shortcuts": true,
	"container": true, "codesign": true, "pkgutil": true, "spctl": true, "hdiutil": true, "ditto": true,
	"plutil": true, "caffeinate": true, "pmset": true, "scutil": true, "ioreg": true, "lipo": true,
	"security": true, "defaults": true, "sw_vers": true, "xattr": true,
}

// hpAmbiguous are tool names that are common words: as the first word of a []string they prove nothing.
var hpAmbiguous = map[string]bool{"open": true, "security": true, "defaults": true}

var (
	hpLibraryPath = regexp.MustCompile(`(^|/)Library(/|$)`)
	hpPathLit     = map[string]string{
		"Library": "Library", "Info.plist": "Info.plist", "MacOS": "MacOS", "LaunchAgents": "LaunchAgents",
	}
	hpArchSuffix = regexp.MustCompile(`_darwin(_(amd64|arm64))?$`)
)

type hpFinding struct {
	file, item, why string
}

// hpMatch says whether a file of the set builds for goos (on amd64 or on arm64), by its name and its constraint.
func hpMatch(files map[string]string, rel, goos string) bool {
	for _, arch := range []string{"amd64", "arm64"} {
		ctx := build.Default
		ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = goos, arch, false
		ctx.BuildTags = nil
		ctx.JoinPath = path.Join
		ctx.OpenFile = func(p string) (io.ReadCloser, error) {
			s, ok := files[p]
			if !ok {
				return nil, os.ErrNotExist
			}
			return io.NopCloser(strings.NewReader(s)), nil
		}
		ok, err := ctx.MatchFile(path.Dir(rel), path.Base(rel))
		if err == nil && ok {
			return true
		}
	}
	return false
}

// hpFuncs is the set of functions and methods a file declares ("name", or "Type.name" for a method).
func hpFuncs(f *ast.File) map[string]bool {
	out := map[string]bool{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := fd.Name.Name
		if fd.Recv != nil && len(fd.Recv.List) == 1 {
			t := fd.Recv.List[0].Type
			for {
				switch x := t.(type) {
				case *ast.StarExpr:
					t = x.X
					continue
				case *ast.IndexExpr:
					t = x.X
					continue
				case *ast.IndexListExpr:
					t = x.X
					continue
				}
				break
			}
			if id, ok := t.(*ast.Ident); ok {
				name = id.Name + "." + name
			}
		}
		out[name] = true
	}
	return out
}

// hpConsts are the package-level string constants and variables set to a literal, over all files of one directory.
func hpConsts(parsed map[string]*ast.File, dir string) map[string]string {
	out := map[string]string{}
	for rel, f := range parsed {
		if path.Dir(rel) != dir {
			continue
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						if bl, ok := vs.Values[i].(*ast.BasicLit); ok && bl.Kind == token.STRING {
							if v, err := strconv.Unquote(bl.Value); err == nil {
								out[n.Name] = v
							}
						}
					}
				}
			}
		}
	}
	return out
}

// hpStr is the value of a string literal, or of an identifier that names a string constant of the package.
func hpStr(e ast.Expr, consts map[string]string) string {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			v, _ := strconv.Unquote(x.Value)
			return v
		}
	case *ast.Ident:
		return consts[x.Name]
	}
	return ""
}

// hpLiteral says what is macOS-only about a string, or "".
func hpLiteral(s string) string {
	if s == "" || len(s) > 4096 {
		return ""
	}
	if strings.HasPrefix(s, "/") && !strings.ContainsAny(s, " \t\n") && hpMacTools[path.Base(s)] {
		return s
	}
	if item, ok := hpPathLit[s]; ok {
		return item
	}
	if strings.Contains(s, "do shell script") || strings.Contains(s, "with administrator privileges") {
		return "AppleScript"
	}
	if strings.Contains(s, "<plist") || strings.Contains(s, "DTD PLIST") {
		return "property list"
	}
	if strings.HasPrefix(s, "kern.") && !strings.ContainsAny(s, " \t\n") {
		return s
	}
	if strings.Contains(s, "Application Support") {
		return "Application Support"
	}
	if !strings.ContainsAny(s, " \t\n") && hpLibraryPath.MatchString(s) {
		return "Library path"
	}
	if strings.HasSuffix(s, ".plist") && !strings.ContainsAny(s, " \t\n") {
		return ".plist file"
	}
	return ""
}

// hpScan returns the macOS-only items of one parsed file, one finding per item.
func hpScan(rel string, f *ast.File, consts map[string]string) []hpFinding {
	seen := map[string]string{}
	add := func(item, why string) {
		if item != "" {
			if _, ok := seen[item]; !ok {
				seen[item] = why
			}
		}
	}
	isIdent := func(e ast.Expr, name string) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == name
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ImportSpec:
			return false // an import path is never a macOS path
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				if v, err := strconv.Unquote(x.Value); err == nil {
					add(hpLiteral(v), "a macOS path or program")
				}
			}
		case *ast.CallExpr:
			if se, ok := x.Fun.(*ast.SelectorExpr); ok && isIdent(se.X, "exec") {
				arg := -1
				switch se.Sel.Name {
				case "Command", "LookPath":
					arg = 0
				case "CommandContext":
					arg = 1
				}
				if arg >= 0 && arg < len(x.Args) {
					if v := hpStr(x.Args[arg], consts); v != "" && hpMacTools[path.Base(v)] {
						add(v, "exec of a macOS-only program")
					}
				}
			}
		case *ast.CompositeLit:
			if at, ok := x.Type.(*ast.ArrayType); ok && at.Len == nil && isIdent(at.Elt, "string") && len(x.Elts) > 0 {
				if v := hpStr(x.Elts[0], consts); v != "" && hpMacTools[v] && !hpAmbiguous[v] {
					add(v, "argv of a macOS-only program")
				}
			}
		case *ast.SelectorExpr:
			if pkg, ok := x.X.(*ast.Ident); ok {
				name := x.Sel.Name
				switch {
				case pkg.Name == "unix" && (strings.HasPrefix(name, "Sysctl") || strings.HasPrefix(name, "LOCAL_") ||
					name == "SOL_LOCAL" || strings.Contains(name, "Xucred")):
					add("unix."+name, "a macOS kernel interface")
				case pkg.Name == "agent" && (name == "AppleContainer" || name == "ContainerBin") && !strings.HasPrefix(rel, "internal/home/agent/"):
					add("agent."+name, "Apple's container tool as the VM")
				}
			}
		}
		return true
	})
	var out []hpFinding
	for item, why := range seen {
		out = append(out, hpFinding{file: rel, item: item, why: why})
	}
	return out
}

// hpCheck runs both checks over a set of sources (slash path from the root -> source).
func hpCheck(files map[string]string) ([]hpFinding, error) {
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	var names []string
	for rel, src := range files {
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			return nil, err
		}
		parsed[rel] = f
		names = append(names, rel)
	}
	sort.Strings(names)
	darwinOnly := map[string]bool{}
	buildsElsewhere := map[string]map[string]bool{"linux": {}, "windows": {}}
	for _, rel := range names {
		lin, win := hpMatch(files, rel, "linux"), hpMatch(files, rel, "windows")
		buildsElsewhere["linux"][rel], buildsElsewhere["windows"][rel] = lin, win
		if !lin && !win && hpMatch(files, rel, "darwin") {
			darwinOnly[rel] = true
		}
	}

	var out []hpFinding
	// 1. mac-only things in a file that builds for another OS
	for _, rel := range names {
		if darwinOnly[rel] || (!buildsElsewhere["linux"][rel] && !buildsElsewhere["windows"][rel]) {
			continue
		}
		out = append(out, hpScan(rel, parsed[rel], hpConsts(parsed, path.Dir(rel)))...)
	}
	// 2. a counterpart for each darwin-only file
	for _, rel := range names {
		if !darwinOnly[rel] {
			continue
		}
		dir, name := path.Dir(rel), strings.TrimSuffix(path.Base(rel), ".go")
		prefix := hpArchSuffix.ReplaceAllString(name, "") + "_"
		want := hpFuncs(parsed[rel])
		for _, goos := range []string{"linux", "windows"} {
			have := map[string]bool{}
			found := false
			for _, other := range names {
				if other == rel || darwinOnly[other] || path.Dir(other) != dir || !buildsElsewhere[goos][other] ||
					!strings.HasPrefix(path.Base(other), prefix) {
					continue
				}
				found = true
				for fn := range hpFuncs(parsed[other]) {
					have[fn] = true
				}
			}
			var missing []string
			for fn := range want {
				if !have[fn] {
					missing = append(missing, fn)
				}
			}
			sort.Strings(missing)
			switch {
			case !found:
				out = append(out, hpFinding{rel, "counterpart:" + goos, "has no " + goos + " counterpart (a " + prefix + goos + ".go, or a " + prefix + "other.go that builds for " + goos + ")"})
			case len(missing) > 0:
				out = append(out, hpFinding{rel, "counterpart:" + goos, "the " + goos + " counterpart does not declare: " + strings.Join(missing, ", ")})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].item < out[j].item
	})
	return out, nil
}

// hpLoad reads the Go sources of the Home packages under base.
func hpLoad(t *testing.T, base string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, d := range hpDirs {
		_ = filepath.WalkDir(filepath.Join(base, filepath.FromSlash(d)), func(p string, de os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if de.IsDir() {
				if de.Name() == "testdata" || de.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				t.Errorf("%s: %v", p, rerr)
				return nil
			}
			rel, _ := filepath.Rel(base, p)
			files[filepath.ToSlash(rel)] = string(b)
			return nil
		})
	}
	return files
}

type hpAllow struct {
	file, item, stage, reason string
	used                      bool
}

// hpReadAllow reads testdata/homeplatform_allow.txt: "file || item || Pn: reason" per line.
func hpReadAllow(t *testing.T, base string) []*hpAllow {
	t.Helper()
	fh, err := os.Open(filepath.Join(base, "internal/repocheck/testdata/homeplatform_allow.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	var out []*hpAllow
	sc := bufio.NewScanner(fh)
	stage := regexp.MustCompile(`^(P\d+): (\S.*)$`)
	for n := 1; sc.Scan(); n++ {
		ln := strings.TrimSpace(sc.Text())
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		p := strings.Split(ln, " || ")
		if len(p) != 3 || p[0] == "" || p[1] == "" || !stage.MatchString(p[2]) {
			t.Fatalf("homeplatform_allow.txt:%d: want `file || item || Pn: reason`", n)
		}
		m := stage.FindStringSubmatch(p[2])
		out = append(out, &hpAllow{file: p[0], item: p[1], stage: m[1], reason: m[2]})
	}
	return out
}

// hpPlanStages are the stage headings of the plan ("### P3 ...").
func hpPlanStages(t *testing.T, base string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(base, "docs/plans/home-platforms.md"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^#{2,4} (P\d+)\b`).FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = true
	}
	return out
}

func TestHomePlatformNoMacOnlyCode(t *testing.T) {
	base := root(t)
	files := hpLoad(t, base)
	// the scan must see the Home packages and the darwin files in them, or a refactor could blind it
	if len(files) < 50 || files["internal/home/peer_darwin.go"] == "" {
		t.Fatalf("read only %d Home sources: the scan would see nothing", len(files))
	}
	found, err := hpCheck(files)
	if err != nil {
		t.Fatal(err)
	}
	allow := hpReadAllow(t, base)
	stages := hpPlanStages(t, base)
	if len(stages) == 0 {
		t.Fatal("docs/plans/home-platforms.md has no stage headings (### P1 ...)")
	}
	for _, a := range allow {
		if !stages[a.stage] {
			t.Errorf("homeplatform_allow.txt: %s %q names stage %s, which docs/plans/home-platforms.md does not have", a.file, a.item, a.stage)
		}
	}
	for _, f := range found {
		ok := false
		for _, a := range allow {
			if a.file == f.file && a.item == f.item {
				a.used, ok = true, true
			}
		}
		if !ok {
			t.Errorf("%s: %s (%q) works on macOS only.\n\tPut the macOS part in a _darwin.go file and give it a _linux.go and _windows.go (or a !darwin _other.go) with the same functions; see docs/plans/home-platforms.md. If it cannot move yet, add `%s || %s || Pn: reason` to internal/repocheck/testdata/homeplatform_allow.txt",
				f.file, f.why, f.item, f.file, f.item)
		}
	}
	for _, a := range allow {
		if !a.used {
			t.Errorf("homeplatform_allow.txt lists %s %q (%s), but nothing matches it any more: remove the line", a.file, a.item, a.stage)
		}
	}
}

// The checker itself, on synthetic sources: what it must catch and what it must leave alone.
func TestHomePlatformChecker(t *testing.T) {
	const (
		osa = "package home\nimport \"os/exec\"\nfunc Dialog() { exec.Command(\"/usr/bin/osascript\", \"-e\", \"x\").Run() }\nfunc (h *H) Show() {}\n"
		fn  = "package home\nfunc Dialog() {}\nfunc (h *H) Show() {}\n"
	)
	keys := func(fs []hpFinding) string {
		var s []string
		for _, f := range fs {
			s = append(s, f.file+":"+f.item)
		}
		return strings.Join(s, ",")
	}
	run := func(files map[string]string) string {
		t.Helper()
		fs, err := hpCheck(files)
		if err != nil {
			t.Fatal(err)
		}
		return keys(fs)
	}

	// an untagged osascript call is a finding
	if got := run(map[string]string{"internal/home/x.go": osa}); got != "internal/home/x.go:/usr/bin/osascript" {
		t.Errorf("untagged osascript: got %q", got)
	}
	// a _test.go file is not read
	if got := run(map[string]string{"internal/home/x_test.go": osa}); got != "" {
		t.Errorf("a test file was read: %q", got)
	}
	// the same call in a darwin-only file is fine, but then the file needs its counterparts
	if got := run(map[string]string{"internal/home/x_darwin.go": osa}); got != "internal/home/x_darwin.go:counterpart:linux,internal/home/x_darwin.go:counterpart:windows" {
		t.Errorf("darwin file without counterpart: got %q", got)
	}
	// ... a //go:build darwin file counts as darwin-only too
	if got := run(map[string]string{"internal/home/y.go": "//go:build darwin\n\n" + osa}); got != "internal/home/y.go:counterpart:linux,internal/home/y.go:counterpart:windows" {
		t.Errorf("constrained darwin file: got %q", got)
	}
	// _linux.go + _windows.go
	if got := run(map[string]string{"internal/home/x_darwin.go": osa, "internal/home/x_linux.go": fn, "internal/home/x_windows.go": fn}); got != "" {
		t.Errorf("full set: got %q", got)
	}
	// one !darwin file covers both
	if got := run(map[string]string{"internal/home/x_darwin.go": osa, "internal/home/x_other.go": "//go:build !darwin\n\n" + fn}); got != "" {
		t.Errorf("!darwin counterpart: got %q", got)
	}
	// !darwin && !linux covers windows only: linux is missing
	if got := run(map[string]string{"internal/home/x_darwin.go": osa, "internal/home/x_other.go": "//go:build !darwin && !linux\n\n" + fn}); got != "internal/home/x_darwin.go:counterpart:linux" {
		t.Errorf("windows-only counterpart: got %q", got)
	}
	// a counterpart that lacks a function or a method is not one
	fs, _ := hpCheck(map[string]string{"internal/home/x_darwin.go": osa, "internal/home/x_linux.go": "package home\nfunc Dialog() {}\n", "internal/home/x_windows.go": fn})
	if len(fs) != 1 || fs[0].item != "counterpart:linux" || !strings.Contains(fs[0].why, "H.Show") {
		t.Errorf("a missing method: got %+v", fs)
	}
	// a counterpart of another name (peerexe_ is not peer_) is not one
	if got := run(map[string]string{"internal/home/peer_darwin.go": fn, "internal/home/peerexe_linux.go": fn, "internal/home/peerexe_windows.go": fn}); got !=
		"internal/home/peer_darwin.go:counterpart:linux,internal/home/peer_darwin.go:counterpart:windows" {
		t.Errorf("another name: got %q", got)
	}
	// a file that builds on Linux is not exempt just because it is not Windows
	if got := run(map[string]string{"internal/home/z.go": "//go:build !windows\n\n" + osa}); got != "internal/home/z.go:/usr/bin/osascript" {
		t.Errorf("!windows file: got %q", got)
	}

	// the other kinds of item
	for name, c := range map[string]struct{ src, item string }{
		"lookpath":    {`package p; import "os/exec"; func f() { exec.LookPath("container") }`, "container"},
		"const":       {`package p; import "os/exec"; const lc = "launchctl"; func f() { exec.CommandContext(nil, lc, "print") }`, "launchctl"},
		"argv":        {`package p; var a = []string{"pbcopy", "-x"}`, "pbcopy"},
		"abs path":    {`package p; var a = "/bin/launchctl"`, "/bin/launchctl"},
		"library":     {`package p; import "path/filepath"; var a = filepath.Join("h", "Library", "x")`, "Library"},
		"lib abs":     {`package p; var a = "/Library/Application Support/Rubi Home"`, "Application Support"},
		"sysctl":      {`package p; var a = "kern.procargs2"`, "kern.procargs2"},
		"unix":        {`package p; import "golang.org/x/sys/unix"; func f() { unix.SysctlRaw("a", 1) }`, "unix.SysctlRaw"},
		"peer":        {`package p; import "golang.org/x/sys/unix"; var a = unix.LOCAL_PEERPID`, "unix.LOCAL_PEERPID"},
		"applescript": {"package p\nvar a = `do shell script \"x\"`", "AppleScript"},
		"plist":       {"package p\nvar a = `<plist version=\"1.0\">`", "property list"},
		"bundle":      {`package p; var a = "Info.plist"`, "Info.plist"},
		"apple vm":    {`package p; import "x/agent"; var a = agent.AppleContainer{}`, "agent.AppleContainer"},
	} {
		if got := run(map[string]string{"internal/home/q.go": c.src}); got != "internal/home/q.go:"+c.item {
			t.Errorf("%s: got %q, want the item %q", name, got, c.item)
		}
	}
	// the agent package may name its own Apple backend
	if got := run(map[string]string{"internal/home/agent/a.go": `package agent; var a = AppleContainer{}; var b = agent.ContainerBin`}); got != "" {
		t.Errorf("inside the agent package: got %q", got)
	}
	// words that are not macOS things
	for name, src := range map[string]string{
		"case open":  `package p; func f(s string) { switch s { case "open": } }`,
		"argv open":  `package p; var a = []string{"open", "x"}`,
		"lower":      `package p; var a = "the library key"`,
		"import":     `package p; import "x/Library/y"`,
		"linux tool": `package p; import "os/exec"; func f() { exec.Command("/usr/bin/systemctl") }`,
		"text":       `package p; var a = "Rubi Home needs the Library folder"`,
	} {
		if got := run(map[string]string{"internal/home/q.go": src}); got != "" {
			t.Errorf("%s: caught by mistake: %q", name, got)
		}
	}
}
