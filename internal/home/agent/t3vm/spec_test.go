package t3vm

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/t3"
)

// testSpec makes a spec over real directories (the checks follow symlinks): base is the real path of a temporary
// directory (macOS puts them behind a symlink).
func testSpec(t *testing.T) SessionSpec {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := SessionSpec{
		Bin: "container", Image: "rubi-agent-runner:abc", Name: "rubi-t3-r1-0011223344556677", Role: "r1",
		HelperDir:   filepath.Join(base, "h"),
		ClaudeDir:   filepath.Join(base, "h/agent-roles/r1/claude"),
		ContentDir:  filepath.Join(base, "h/agent-roles/r1/content"),
		SignInDir:   filepath.Join(base, "h/state/agent-signin"),
		TrustedRoot: filepath.Join(base, "projects"),
		Cwd:         filepath.Join(base, "projects/proj"),
		ReadOnly:    []string{filepath.Join(base, "t3/userdata/attachments")},
		HookScript:  filepath.Join(base, "h/t3/rubi-hook.mjs"), ManagedSettings: filepath.Join(base, "h/t3/managed-settings.json"), T3Port: 4410,
	}
	for _, d := range append([]string{s.HelperDir, s.ClaudeDir, s.ContentDir, s.SignInDir, s.TrustedRoot, s.Cwd}, s.ReadOnly...) {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{s.HookScript, s.ManagedSettings} {
		os.MkdirAll(filepath.Dir(f), 0o700)
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestNewNameIsUniqueAndSafe(t *testing.T) {
	re := regexp.MustCompile(`^rubi-t3-[a-z0-9-]+-[0-9a-f]{16}$`)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		n, err := NewName("Role_1/../x y")
		if err != nil || !re.MatchString(n) {
			t.Fatalf("%q %v", n, err)
		}
		if seen[n] {
			t.Fatalf("name repeated: %s", n)
		}
		seen[n] = true
	}
	if n, _ := NewName(strings.Repeat("a", 80)); len(n) > len("rubi-t3-")+24+1+16 {
		t.Errorf("long role id not cut: %s", n)
	}
	if _, err := NewName("///"); err == nil {
		t.Error("a role id with nothing usable was accepted")
	}
}

func TestRunArgsIsTheLockedDownVMWithSamePathMounts(t *testing.T) {
	s := testSpec(t)
	got, err := s.RunArgs()
	if err != nil {
		t.Fatal(err)
	}
	rw := func(p string) string { return "type=bind,source=" + p + ",target=" + p }
	ro := func(p string) string { return rw(p) + ",readonly" }
	want := []string{"container", "run", "-d", "--name", "rubi-t3-r1-0011223344556677", "--init",
		"--network", "none", "--user", "10001", "--cap-drop", "ALL", "--read-only", "--tmpfs", "/tmp",
		"-v", s.ManagedSettings + ":/etc/claude-code/managed-settings.json:ro",
		"-v", s.HookScript + ":/etc/rubi/rubi-hook.mjs:ro",
		"--workdir", s.Cwd, "--entrypoint", "sleep", "rubi-agent-runner:abc", "infinity"}
	// the mounts are ordered by depth; compare the fixed parts exactly and the mounts as a set with their flags
	if !reflect.DeepEqual(fixed(got), want) {
		t.Fatalf("got  %q\nwant %q", fixed(got), want)
	}
	wantMounts := map[string]bool{rw(s.Cwd): true, rw(s.ClaudeDir): true, rw(s.SignInDir): true, ro(s.ContentDir): true, ro(s.ReadOnly[0]): true}
	if !reflect.DeepEqual(mounts(got), wantMounts) {
		t.Fatalf("mounts %v\nwant   %v", mounts(got), wantMounts)
	}
}

func fixed(a []string) []string {
	var out []string
	for i := 0; i < len(a); i++ {
		if a[i] == "--mount" {
			i++
			continue
		}
		out = append(out, a[i])
	}
	return out
}

func mounts(a []string) map[string]bool {
	m := map[string]bool{}
	for i := 0; i+1 < len(a); i++ {
		if a[i] == "--mount" {
			m[a[i+1]] = true
		}
	}
	return m
}

func mountOrder(got []string) []string {
	var order []string
	for i, a := range got {
		if a == "--mount" {
			order = append(order, strings.Split(strings.TrimPrefix(got[i+1], "type=bind,source="), ",")[0])
		}
	}
	return order
}

// A read-only mount that lies inside a read-write one must come after it, or it is hidden by it.
func TestRunArgsMountsParentsBeforeWhatLiesInside(t *testing.T) {
	s := testSpec(t)
	att := filepath.Join(s.Cwd, "deeper", "att")
	if err := os.MkdirAll(att, 0o700); err != nil {
		t.Fatal(err)
	}
	s.ReadOnly = []string{att}
	got, err := s.RunArgs()
	if err != nil {
		t.Fatal(err)
	}
	order := mountOrder(got)
	idx := func(p string) int {
		for i, o := range order {
			if o == p {
				return i
			}
		}
		return -1
	}
	if !(idx(s.Cwd) >= 0 && idx(s.Cwd) < idx(att)) {
		t.Fatalf("mount order: %q", order)
	}
}

func TestRunArgsRefusesBadPathsAndConflicts(t *testing.T) {
	for name, mod := range map[string]func(*SessionSpec){
		"relative cwd":          func(s *SessionSpec) { s.Cwd = "w/proj" },
		"comma in cwd":          func(s *SessionSpec) { s.Cwd = s.Cwd + ",x" },
		"colon in hook":         func(s *SessionSpec) { s.HookScript = s.HookScript + ":x" },
		"root as cwd":           func(s *SessionSpec) { s.Cwd = "/" },
		"unclean path":          func(s *SessionSpec) { s.SignInDir = s.SignInDir + "/../agent-signin" },
		"empty content":         func(s *SessionSpec) { s.ContentDir = "" },
		"missing cwd":           func(s *SessionSpec) { s.Cwd = s.Cwd + "-gone" },
		"name not ours":         func(s *SessionSpec) { s.Name = "rubi-agent-x" },
		"no image":              func(s *SessionSpec) { s.Image = "" },
		"no trusted root":       func(s *SessionSpec) { s.TrustedRoot = "" },
		"no helper dir":         func(s *SessionSpec) { s.HelperDir = "" },
		"rw and ro same place":  func(s *SessionSpec) { s.ReadOnly = []string{s.Cwd} },
		"newline in attachment": func(s *SessionSpec) { s.ReadOnly = []string{s.Cwd + "/a\nb"} },
	} {
		s := testSpec(t)
		mod(&s)
		if _, err := s.RunArgs(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// the same directory twice, in the same mode, is one mount
	s := testSpec(t)
	s.ReadOnly = []string{s.ContentDir}
	got, err := s.RunArgs()
	if err != nil || strings.Count(strings.Join(got, " "), "target="+s.ContentDir) != 1 {
		t.Errorf("duplicate mount: %v %q", err, got)
	}
}

func TestRunArgsRefusesPortsThatCollideOrArePrivileged(t *testing.T) {
	for _, port := range []int{-1, 1, 80, 1023, 3128, 3129, 3130, 65536} {
		s := testSpec(t)
		s.T3Port = port
		if _, err := s.RunArgs(); err == nil {
			t.Errorf("port %d accepted", port)
		}
	}
	for _, port := range []int{0, 1024, 3127, 3131, 4410, 65535} {
		s := testSpec(t)
		s.T3Port = port
		if _, err := s.RunArgs(); err != nil {
			t.Errorf("port %d refused: %v", port, err)
		}
	}
}

// The workspace is followed to its real place: a link inside the project that leaves the trusted root is refused,
// one that stays inside is mounted from where it really is.
func TestRunArgsFollowsSymlinksOfTheWorkspace(t *testing.T) {
	s := testSpec(t)
	outside := filepath.Join(filepath.Dir(s.TrustedRoot), "elsewhere")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.TrustedRoot, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	s.Cwd = link
	if _, err := s.RunArgs(); err == nil || !strings.Contains(err.Error(), "not inside the trusted project root") {
		t.Fatalf("a link out of the trusted root: %v", err)
	}
	// a link to Home's own directory
	home := filepath.Join(s.TrustedRoot, "home-link")
	if err := os.Symlink(s.HelperDir, home); err != nil {
		t.Fatal(err)
	}
	s.Cwd = home
	if _, err := s.RunArgs(); err == nil {
		t.Fatal("a link into Rubi Home's directory was accepted")
	}
	// a link that stays inside: mounted from the real place, at the path T3 uses
	real := filepath.Join(s.TrustedRoot, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(s.TrustedRoot, "inside")
	if err := os.Symlink(real, inside); err != nil {
		t.Fatal(err)
	}
	s.Cwd = inside
	got, err := s.RunArgs()
	if err != nil {
		t.Fatal(err)
	}
	if !mounts(got)["type=bind,source="+real+",target="+inside] {
		t.Fatalf("mounts: %v", mounts(got))
	}
	// the trusted root itself, and a root that does not hold the folder
	s.Cwd = s.TrustedRoot
	if _, err := s.RunArgs(); err != nil {
		t.Errorf("the root itself: %v", err)
	}
	s.TrustedRoot = real
	s.Cwd = inside
	if _, err := s.RunArgs(); err != nil {
		t.Errorf("a link to the root: %v", err)
	}
	s.Cwd = filepath.Join(filepath.Dir(real), "inside-other")
	os.MkdirAll(s.Cwd, 0o700)
	if _, err := s.RunArgs(); err == nil {
		t.Error("a folder beside the root was accepted")
	}
}

// A root whose name only starts like the folder's must not count as holding it.
func TestRunArgsTrustedRootIsNotAPrefixMatch(t *testing.T) {
	s := testSpec(t)
	sibling := s.TrustedRoot + "-other"
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	s.Cwd = sibling
	if _, err := s.RunArgs(); err == nil {
		t.Fatal("projects-other accepted under projects")
	}
}

func TestRunArgsKeepsHomesFilesOutOfReadWriteMounts(t *testing.T) {
	for name, mod := range map[string]func(*SessionSpec){
		"workspace holds Home's directory":  func(s *SessionSpec) { s.TrustedRoot = filepath.Dir(s.HelperDir); s.Cwd = filepath.Dir(s.HelperDir) },
		"workspace inside Home's directory": func(s *SessionSpec) { s.TrustedRoot = s.HelperDir; s.Cwd = filepath.Join(s.HelperDir, "agent-roles") },
		"workspace elsewhere in Home's directory": func(s *SessionSpec) {
			s.TrustedRoot = s.HelperDir
			s.Cwd = filepath.Join(s.HelperDir, "t3-state")
			os.MkdirAll(s.Cwd, 0o700)
		},
		"workspace holds settings kept outside Home": func(s *SessionSpec) {
			ext := filepath.Join(filepath.Dir(s.HelperDir), "ext")
			os.MkdirAll(ext, 0o700)
			s.ManagedSettings = filepath.Join(ext, "m.json")
			os.WriteFile(s.ManagedSettings, []byte("x"), 0o600)
			s.TrustedRoot, s.Cwd = ext, ext
		},
		"workspace holds the hook kept outside Home": func(s *SessionSpec) {
			ext := filepath.Join(filepath.Dir(s.HelperDir), "ext2")
			os.MkdirAll(ext, 0o700)
			s.HookScript = filepath.Join(ext, "h.mjs")
			os.WriteFile(s.HookScript, []byte("x"), 0o600)
			s.TrustedRoot, s.Cwd = ext, ext
		},
		"workspace holds the managed settings": func(s *SessionSpec) {
			s.TrustedRoot = filepath.Dir(s.ManagedSettings)
			s.Cwd = filepath.Dir(s.ManagedSettings)
		},
		"workspace is the sign-in's state dir": func(s *SessionSpec) { s.TrustedRoot = filepath.Dir(s.SignInDir); s.Cwd = filepath.Dir(s.SignInDir) },
		"workspace is the role's claude dir":   func(s *SessionSpec) { s.TrustedRoot = s.ClaudeDir; s.Cwd = s.ClaudeDir },
		"workspace is the role's directory":    func(s *SessionSpec) { s.TrustedRoot = filepath.Dir(s.ClaudeDir); s.Cwd = filepath.Dir(s.ClaudeDir) },
		"workspace is the content dir":         func(s *SessionSpec) { s.TrustedRoot = s.ContentDir; s.Cwd = s.ContentDir },
		"claude dir holds the hook script": func(s *SessionSpec) {
			s.HookScript = filepath.Join(s.ClaudeDir, "hook.mjs")
			os.WriteFile(s.HookScript, []byte("x"), 0o600)
		},
		"sign-in dir holds the settings": func(s *SessionSpec) {
			s.ManagedSettings = filepath.Join(s.SignInDir, "m.json")
			os.WriteFile(s.ManagedSettings, []byte("x"), 0o600)
		},
		"claude dir holds Home's directory": func(s *SessionSpec) { s.ClaudeDir = filepath.Dir(s.HelperDir) },
		"claude dir holds the sign-in dir": func(s *SessionSpec) {
			s.SignInDir = filepath.Join(s.ClaudeDir, "signin")
			os.MkdirAll(s.SignInDir, 0o700)
		},
	} {
		s := testSpec(t)
		mod(&s)
		if _, err := s.RunArgs(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// the role's claude dir and the sign-in dir inside Home's directory are fine (that is where they live)
	if _, err := testSpec(t).RunArgs(); err != nil {
		t.Fatal(err)
	}
}

func TestExecArgs(t *testing.T) {
	s := testSpec(t)
	c, err := ClampArgv([]string{"--verbose", "--model", "m"}, pol(t3.Auto))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ExecArgs("/h/env/e1", "/opt/claude", c)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"container", "exec", "-i", "--user", "10001", "--workdir", s.Cwd, "--env-file", "/h/env/e1",
		"rubi-t3-r1-0011223344556677", "/opt/claude", "--verbose", "--model", "m", "--permission-mode", "default", "--strict-mcp-config"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	if _, err := s.ExecArgs("", "/opt/claude", c); err == nil {
		t.Error("an exec without an env file was built")
	}
}

func TestStopDeleteAndPipeEnv(t *testing.T) {
	s := testSpec(t)
	if !reflect.DeepEqual(s.StopArgs(), []string{"container", "stop", "--time", "5", s.Name}) ||
		!reflect.DeepEqual(s.DeleteArgs(), []string{"container", "delete", "--force", s.Name}) {
		t.Errorf("stop/delete: %q %q", s.StopArgs(), s.DeleteArgs())
	}
	if !reflect.DeepEqual(s.PipeEnv(), map[string]string{"RUBI_T3_PORT": "4410"}) {
		t.Errorf("pipe env: %v", s.PipeEnv())
	}
	s.T3Port = 0
	if s.PipeEnv() != nil {
		t.Errorf("pipe env without a port: %v", s.PipeEnv())
	}
}
