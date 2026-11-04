// Package t3vm is the Rubi Home side of running T3 Code's `claude` in a role VM on the user's PC (docs/plans/t3-
// integration.md section 12): the pure pieces that decide what a session VM looks like (its `container run` and
// `container exec` command lines, its environment, T3's argument list after the role's ceiling is applied, the
// managed settings and the hook that sends every tool call to Rubi Home) and the hook gate that answers it. Nothing
// here starts a process or opens a socket; the callers do, and every policy lives here so a test can pin it.
package t3vm

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Where the VM sees the files Home provides (single files, mounted read-only).
const (
	// ManagedSettingsVMPath is where Claude Code reads the policy that a project or a user can't override.
	ManagedSettingsVMPath = "/etc/claude-code/managed-settings.json"
	// HookVMPath is the PreToolUse hook script (HookScript).
	HookVMPath = "/etc/rubi/rubi-hook.mjs"
)

// uid is the image's non-root user (runner/container/Dockerfile).
const uid = "10001"

var nameUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// NewName makes the unique name of a session's container: rubi-t3-<role>-<8 random bytes in hex>.
func NewName(role string) (string, error) {
	slug := strings.Trim(nameUnsafe.ReplaceAllString(strings.ToLower(role), "-"), "-")
	if len(slug) > 24 {
		slug = strings.TrimRight(slug[:24], "-")
	}
	if slug == "" {
		return "", errors.New("t3vm: the role has no usable id")
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "rubi-t3-" + slug + "-" + hex.EncodeToString(b), nil
}

// SessionSpec is everything the VM of one T3 provider session is made from. Every directory is mounted at its host
// path (T3 and its history worker see the same paths as the claude inside).
type SessionSpec struct {
	// Bin is the VM tool's path (the backend's; required) and Image the runner image's tag.
	Bin   string
	Image string
	// Name is the container's name (NewName).
	Name string
	// Role is the role's id; ClaudeDir its config directory (read-write) and ContentDir its content (read-only).
	Role       string
	ClaudeDir  string
	ContentDir string
	// Cwd is the thread's workspace (read-write) and the VM's working directory. Its real path (symlinks followed)
	// must be TrustedRoot or lie inside it, and it must not overlap anything of Rubi Home's (see Validate).
	Cwd string
	// TrustedRoot is the folder the role is pinned to (project trust): the real path of Cwd must be inside it.
	TrustedRoot string
	// HelperDir is Rubi Home's own directory. No read-write mount of the workspace may contain, equal or lie inside
	// it, except the role's claude directory and the sign-in directory, which are Home's own.
	HelperDir string
	// SignInDir is the sign-in folder shared by all roles (read-write: claude rotates the refresh token in it, so
	// the VM can read the credential; the managed settings' deny rules for it are best effort, see ManagedSettings).
	SignInDir string
	// ReadOnly are more directories, mounted read-only: T3's attachments folder, which argv names with --add-dir.
	ReadOnly []string
	// HookScript and ManagedSettings are host files, shown read-only at HookVMPath and ManagedSettingsVMPath.
	HookScript      string
	ManagedSettings string
	// T3Port is the port of T3's own MCP server on the host, which the VM's forwarder listens on (loopback).
	T3Port int
	// CPUs and Memory limit the VM ("" = the CLI's defaults).
	CPUs   string
	Memory string
}

// bin is the VM tool's path, which the host (the backend) gives; the spec never names a tool itself.
func (s SessionSpec) bin() string { return s.Bin }

// checkPath refuses a path that can't be put into a mount option or a volume argument safely.
func checkPath(what, p string) error {
	if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return fmt.Errorf("t3vm: %s must be a clean absolute path", what)
	}
	if p == "/" || strings.ContainsAny(p, ",:\n\r\x00") {
		return fmt.Errorf("t3vm: %s can't be mounted", what)
	}
	return nil
}

// realPath is the path with every symlink followed; the place must exist.
func real(what, p string) (string, error) {
	if err := checkPath(what, p); err != nil {
		return "", err
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("t3vm: %s: %w", what, err)
	}
	return r, checkPath(what, r)
}

// within: child is parent or lies inside it (both clean and absolute).
func within(parent, child string) bool {
	return child == parent || strings.HasPrefix(child, strings.TrimSuffix(parent, "/")+"/")
}

// overlap: one of the two contains the other.
func overlap(a, b string) bool { return within(a, b) || within(b, a) }

// bind is one directory mount: src is the real place on the Mac, dst where the VM sees it (the path T3 uses).
type bind struct {
	src, dst string
	readOnly bool
}

// resolved is the spec with every path followed to its real place.
type resolved struct {
	binds                                        []bind
	cwd, claude, signIn, content, settings, hook string
}

// resolve checks the paths and the mounts' relations to each other and to Rubi Home's files. Docs: the workspace
// must be inside the trusted root after symlinks are followed (a link in a project can't point a session at the
// rest of the disk); no read-write mount may contain Home's managed settings or hook (the VM would rewrite its own
// policy), and the workspace may not overlap Home's directory, the role's directories or the sign-in's parent.
func (s SessionSpec) resolve() (*resolved, error) {
	var r resolved
	var err error
	for _, d := range []struct {
		what, in string
		out      *string
	}{{"the working directory", s.Cwd, &r.cwd}, {"the role's claude directory", s.ClaudeDir, &r.claude},
		{"the sign-in directory", s.SignInDir, &r.signIn}, {"the role's content directory", s.ContentDir, &r.content},
		{"the managed settings file", s.ManagedSettings, &r.settings}, {"the hook script", s.HookScript, &r.hook}} {
		if *d.out, err = real(d.what, d.in); err != nil {
			return nil, err
		}
	}
	root, err := real("the trusted project root", s.TrustedRoot)
	if err != nil {
		return nil, err
	}
	helper, err := real("Rubi Home's directory", s.HelperDir)
	if err != nil {
		return nil, err
	}
	if !within(root, r.cwd) {
		return nil, fmt.Errorf("t3vm: the working directory is not inside the trusted project root")
	}
	// the workspace is read-write: nothing of Rubi Home's may be in it or around it
	for _, h := range []struct{ what, path string }{{"Rubi Home's directory", helper}, {"the role's claude directory", r.claude},
		{"the role's directory", filepath.Dir(r.claude)}, {"the sign-in directory", r.signIn}, {"the sign-in's state directory", filepath.Dir(r.signIn)},
		{"the role's content directory", r.content}, {"the managed settings", r.settings}, {"the hook script", r.hook}} {
		if overlap(r.cwd, h.path) {
			return nil, fmt.Errorf("t3vm: the working directory overlaps %s", h.what)
		}
	}
	// the other read-write mounts are Home's own, and may not hold the policy files or each other
	for _, rw := range []struct{ what, path string }{{"the role's claude directory", r.claude}, {"the sign-in directory", r.signIn}} {
		for _, f := range []struct{ what, path string }{{"the managed settings", r.settings}, {"the hook script", r.hook}} {
			if within(rw.path, f.path) {
				return nil, fmt.Errorf("t3vm: %s contains %s", rw.what, f.what)
			}
		}
		if within(rw.path, helper) {
			return nil, fmt.Errorf("t3vm: %s contains Rubi Home's directory", rw.what)
		}
	}
	if overlap(r.claude, r.signIn) {
		return nil, errors.New("t3vm: the role's claude directory and the sign-in directory overlap")
	}
	in := []bind{{r.cwd, s.Cwd, false}, {r.claude, s.ClaudeDir, false}, {r.signIn, s.SignInDir, false}, {r.content, s.ContentDir, true}}
	for _, p := range s.ReadOnly {
		src, err := real("a read-only directory", p)
		if err != nil {
			return nil, err
		}
		in = append(in, bind{src, p, true})
	}
	seen := map[string]bind{}
	for _, b := range in {
		if o, ok := seen[b.dst]; ok {
			if o.readOnly != b.readOnly || o.src != b.src {
				return nil, fmt.Errorf("t3vm: %s is asked for twice, differently", b.dst)
			}
			continue
		}
		seen[b.dst] = b
		r.binds = append(r.binds, b)
	}
	// parents before what lies inside them
	depth := func(p string) int { return strings.Count(p, "/") }
	sort.SliceStable(r.binds, func(i, j int) bool {
		if di, dj := depth(r.binds[i].dst), depth(r.binds[j].dst); di != dj {
			return di < dj
		}
		return r.binds[i].dst < r.binds[j].dst
	})
	return &r, nil
}

// Validate checks everything RunArgs checks.
func (s SessionSpec) Validate() error {
	_, err := s.check()
	return err
}

func (s SessionSpec) check() (*resolved, error) {
	if s.Name == "" || !strings.HasPrefix(s.Name, "rubi-t3-") || nameUnsafe.MatchString(s.Name) {
		return nil, errors.New("t3vm: the container needs a name from NewName")
	}
	if s.Image == "" {
		return nil, errors.New("t3vm: no image")
	}
	if s.Bin == "" {
		return nil, errors.New("t3vm: no VM tool")
	}
	if s.Role == "" {
		return nil, errors.New("t3vm: no role")
	}
	// 0: no T3 service. Otherwise a port that is not privileged and is not one of the forwarder's own.
	if s.T3Port != 0 && (s.T3Port < 1024 || s.T3Port > 65535 || (s.T3Port >= 3128 && s.T3Port <= 3130)) {
		return nil, errors.New("t3vm: bad T3 port")
	}
	return s.resolve()
}

// RunArgs starts the session's VM detached: no network, no capabilities, a read-only root with a tmpfs for /tmp, a
// non-root user, and only the mounts above. The container just waits; claude is started by ExecArgs.
func (s SessionSpec) RunArgs() ([]string, error) {
	args, _, err := s.Plan()
	return args, err
}

// ExpectedMount is a mount the VM must have.
type ExpectedMount struct {
	Source, Destination string
	ReadOnly            bool
}

// Plan is RunArgs with the mounts it asks for (sources as the real places on the Mac, from the same resolution of
// the paths), to compare with what the container tool reports once the VM runs.
func (s SessionSpec) Plan() ([]string, []ExpectedMount, error) {
	r, err := s.check()
	if err != nil {
		return nil, nil, err
	}
	var want []ExpectedMount
	for _, b := range r.binds {
		want = append(want, ExpectedMount{b.src, b.dst, b.readOnly})
	}
	want = append(want, ExpectedMount{r.settings, ManagedSettingsVMPath, true}, ExpectedMount{r.hook, HookVMPath, true})
	args := []string{s.bin(), "run", "-d", "--name", s.Name, "--init",
		"--network", "none", "--user", uid, "--cap-drop", "ALL", "--read-only", "--tmpfs", "/tmp"}
	for _, b := range r.binds {
		opt := "type=bind,source=" + b.src + ",target=" + b.dst
		if b.readOnly {
			opt += ",readonly"
		}
		args = append(args, "--mount", opt)
	}
	// Single files go in with -v, as the S0 spike did (container 1.6.1).
	args = append(args,
		"-v", r.settings+":"+ManagedSettingsVMPath+":ro",
		"-v", r.hook+":"+HookVMPath+":ro",
		"--workdir", s.Cwd)
	if s.CPUs != "" {
		args = append(args, "--cpus", s.CPUs)
	}
	if s.Memory != "" {
		args = append(args, "--memory", s.Memory)
	}
	return append(args, "--entrypoint", "sleep", s.Image, "infinity"), want, nil
}

// ExecArgs runs claude (its path in the image) with the clamped argv in the session's VM, stdio passed through.
// Run it only for a spec that RunArgs accepted.
func (s SessionSpec) ExecArgs(envFile, claude string, argv ClampedArgv) ([]string, error) {
	if err := checkPath("the working directory", s.Cwd); err != nil {
		return nil, err
	}
	if s.Name == "" || envFile == "" || !strings.HasPrefix(claude, "/") {
		return nil, errors.New("t3vm: an exec needs the container, an env file and claude's path")
	}
	args := []string{s.bin(), "exec", "-i", "--user", uid, "--workdir", s.Cwd, "--env-file", envFile, s.Name, claude}
	return append(args, argv.args...), nil
}

// StopArgs and DeleteArgs end the VM when the session does.
func (s SessionSpec) StopArgs() []string {
	return []string{s.bin(), "stop", "--time", "5", s.Name}
}

func (s SessionSpec) DeleteArgs() []string {
	return []string{s.bin(), "delete", "--force", s.Name}
}

// PipeEnv is the environment of the forwarder's exec (rubi-netpipe): the T3 service listens when it has a port.
func (s SessionSpec) PipeEnv() map[string]string {
	if s.T3Port <= 0 {
		return nil
	}
	return map[string]string{"RUBI_T3_PORT": strconv.Itoa(s.T3Port)}
}
