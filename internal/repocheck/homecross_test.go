package repocheck

// Rubi Home must build for Windows as well as for Unix systems (docs/plans/home-platforms.md, stage P1). The CI job
// home-cross cross-compiles and vets it; this test catches the same slip earlier, and without a toolchain for the
// other system: a file that builds for Windows must not name a call, constant or type that exists on Unix only.
// The Unix form goes in a file that builds on Unix only (//go:build unix; the name _unix.go means nothing to Go),
// or behind internal/home/fsx, which has a form for each system.
//
// It reads the Go sources of the Home packages, tests included (a test that does not compile fails vet).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// hpUnixSyscall are the names of package syscall that Windows lacks.
var hpUnixSyscall = map[string]bool{
	"Flock": true, "LOCK_EX": true, "LOCK_NB": true, "LOCK_UN": true, "LOCK_SH": true, "Kill": true, "Stat_t": true,
	"O_NOFOLLOW": true, "Mkfifo": true, "Umask": true, "Exec": true, "Chroot": true, "Statfs": true, "Statfs_t": true,
	"Getpgid": true, "Setsid": true, "Fchmod": true, "Fchown": true, "Lchown": true, "Mknod": true,
	"SIGUSR1": true, "SIGUSR2": true,
}

// hpUnixAttr are the fields of syscall.SysProcAttr that Windows lacks.
var hpUnixAttr = map[string]bool{"Setpgid": true, "Setsid": true, "Setctty": true, "Noctty": true, "Pgid": true, "Credential": true, "Foreground": true, "Pdeathsig": true, "Chroot": true, "Ctty": true}

// hpCrossCheck returns "file: use" for each Unix-only use in a file that builds for Windows.
func hpCrossCheck(files map[string]string) ([]string, error) {
	fset := token.NewFileSet()
	var rels []string
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	var out []string
	for _, rel := range rels {
		if !hpMatch(files, rel, "windows") {
			continue
		}
		f, err := parser.ParseFile(fset, rel, files[rel], 0)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		note := func(use string) {
			if !seen[use] {
				seen[use] = true
				out = append(out, rel+": "+use)
			}
		}
		imports := map[string]string{} // local name -> import path
		for _, im := range f.Imports {
			p := strings.Trim(im.Path.Value, `"`)
			name := filepath.Base(p)
			if im.Name != nil {
				name = im.Name.Name
			}
			imports[name] = p
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok {
					switch imports[id.Name] {
					case "syscall":
						if hpUnixSyscall[x.Sel.Name] {
							note("syscall." + x.Sel.Name)
						}
					case "golang.org/x/sys/unix":
						note("unix." + x.Sel.Name)
					}
				}
			case *ast.CompositeLit:
				if se, ok := x.Type.(*ast.SelectorExpr); ok && se.Sel.Name == "SysProcAttr" {
					for _, e := range x.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok && hpUnixAttr[k.Name] {
								note("SysProcAttr." + k.Name)
							}
						}
					}
				}
			}
			return true
		})
	}
	return out, nil
}

func hpLoadWithTests(t *testing.T, base string) map[string]string {
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
			if strings.HasSuffix(p, ".go") {
				b, rerr := os.ReadFile(p)
				if rerr != nil {
					t.Errorf("%s: %v", p, rerr)
					return nil
				}
				rel, _ := filepath.Rel(base, p)
				files[filepath.ToSlash(rel)] = string(b)
			}
			return nil
		})
	}
	return files
}

func TestHomeBuildsForWindows(t *testing.T) {
	base := root(t)
	files := hpLoadWithTests(t, base)
	if len(files) < 50 || files["internal/home/fsx/fsx_unix.go"] == "" || files["internal/home/fsx/fsx_windows.go"] == "" {
		t.Fatalf("read only %d Home sources: the scan would see nothing", len(files))
	}
	bad, err := hpCrossCheck(files)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bad {
		t.Errorf("%s exists on Unix only, but this file builds for Windows.\n\tUse internal/home/fsx, or move the code to a file with //go:build unix and give it a _windows.go next to it", b)
	}
}

// The CI job that cross-builds Home must stay: it names the three systems, vets and builds.
func TestCIBuildsHomeForThreeSystems(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), ".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	ci := string(b)
	i := strings.Index(ci, "home-cross:")
	if i < 0 {
		t.Fatal("ci.yml has no home-cross job")
	}
	job := ci[i:]
	if j := regexp.MustCompile(`\n  [a-z-]+:\n`).FindStringIndex(job[len("home-cross:"):]); j != nil {
		job = job[:len("home-cross:")+j[0]]
	}
	for _, want := range []string{"linux", "darwin", "windows", "go vet", "go build", "./cmd/rubi-home", "./internal/home/..."} {
		if !strings.Contains(job, want) {
			t.Errorf("the home-cross job of ci.yml does not mention %q", want)
		}
	}
}

func TestHomeCrossCheckerOnSyntheticSources(t *testing.T) {
	run := func(files map[string]string) string {
		t.Helper()
		out, err := hpCrossCheck(files)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, ",")
	}
	flock := "package p\nimport \"syscall\"\nfunc f(fd int) error { return syscall.Flock(fd, syscall.LOCK_EX) }\n"
	if got := run(map[string]string{"internal/home/a.go": flock}); got != "internal/home/a.go: syscall.Flock,internal/home/a.go: syscall.LOCK_EX" {
		t.Errorf("untagged Flock: %q", got)
	}
	// the name _unix.go is no constraint: only the build tag is
	if got := run(map[string]string{"internal/home/a_unix.go": flock}); got == "" {
		t.Error("a file named _unix.go without the tag was taken for Unix-only")
	}
	for name, files := range map[string]map[string]string{
		"unix tag":             {"internal/home/a.go": "//go:build unix\n\n" + flock},
		"darwin":               {"internal/home/a_darwin.go": flock},
		"test file with a tag": {"internal/home/a_test.go": "//go:build !windows\n\n" + flock},
	} {
		if got := run(files); got != "" {
			t.Errorf("%s: caught by mistake: %q", name, got)
		}
	}
	if got := run(map[string]string{"internal/home/a_test.go": "package p\nimport \"syscall\"\nvar _ = syscall.Mkfifo"}); got != "internal/home/a_test.go: syscall.Mkfifo" {
		t.Errorf("an untagged test: %q", got)
	}
	if got := run(map[string]string{"internal/home/a.go": "package p\nimport (\"os/exec\"; \"syscall\")\nvar _ = exec.Cmd{SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}"}); got != "internal/home/a.go: SysProcAttr.Setpgid" {
		t.Errorf("Setpgid: %q", got)
	}
	if got := run(map[string]string{"internal/home/a.go": "package p\nimport \"golang.org/x/sys/unix\"\nvar _ = unix.Getpid"}); got != "internal/home/a.go: unix.Getpid" {
		t.Errorf("x/sys/unix: %q", got)
	}
	if got := run(map[string]string{"internal/home/a.go": "package p\nimport \"syscall\"\nvar _ = syscall.SIGTERM\nvar _ = syscall.EINTR"}); got != "" {
		t.Errorf("portable names caught: %q", got)
	}
}
