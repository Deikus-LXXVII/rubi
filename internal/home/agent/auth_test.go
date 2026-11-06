package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// authFake answers the container CLI; nothing real runs.
type authFake struct {
	mu   sync.Mutex
	cmds [][]string
	ls   string // what `ls --all --format json` prints
}

func (f *authFake) run(ctx context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, argv)
	ls := f.ls
	f.mu.Unlock()
	if len(argv) > 1 && argv[1] == "ls" {
		return []byte(ls), nil
	}
	return nil, nil
}

func (f *authFake) find(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if strings.HasPrefix(strings.Join(c[1:], " "), prefix) {
			return c
		}
	}
	return nil
}

func newAuthVM(t *testing.T, f *authFake) (*AppleContainer, string) {
	env := t.TempDir()
	a := &AppleContainer{Bin: "container", EnvDir: env}
	a.run = f.run
	return a, filepath.Join(env, SignInDirName)
}

// A project VM never has the sign-in folder, or any credential, or anything of it.
func TestProjectVMsNeverMountTheSignInFolder(t *testing.T) {
	a := &AppleContainer{Bin: "container", EnvDir: "/home/alice/Rubi Home"}
	for _, p := range []Project{{ID: "alpha", Dir: "/home/alice/alpha"}, {ID: "beta", Dir: "/home/alice/beta"}} {
		args := strings.Join(a.RunArgs(p), " ")
		for _, bad := range []string{"agent-signin", "agent-auth", "rubi-auth", "credentials", "sk-ant", "oauth", "volume", "readonly"} {
			if strings.Contains(args, bad) {
				t.Errorf("%s: %q in the run arguments: %s", p.ID, bad, args)
			}
		}
		if !strings.Contains(args, "source="+p.Dir+",target=/work") || strings.Count(args, "--mount") != 1 {
			t.Errorf("%s: %s", p.ID, args)
		}
	}
}

func TestStartLeavesTheSignInFolderAlone(t *testing.T) {
	f := &authFake{}
	a, dir := newAuthVM(t, f)
	a.spawn = newVMPipes(t, nil).spawn
	fastPipe(t)
	if _, err := a.Start(context.Background(), StartSpec{Project: Project{ID: "my-project", Dir: "/home/alice/my-project"}, Pipe: &PipeSpec{Proxy: netpipe.NewListener("p", 1)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("starting a project VM created the sign-in folder")
	}
	if f.find("volume") != nil {
		t.Error("a volume command ran")
	}
}

func TestStartLoginMountsOnlyTheSharedSignInFolderAtItsOwnPath(t *testing.T) {
	f := &authFake{}
	a, _ := newAuthVM(t, f)
	fastPipe(t)
	a.spawn = newVMPipes(t, nil).spawn
	dir := filepath.Join(a.EnvDir, SignInDirName)
	s, err := a.StartLogin(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	real, _ := filepath.EvalSymlinks(dir)
	run := strings.Join(f.find("run -d"), " ")
	name := ""
	for i, x := range f.find("run -d") {
		if x == "--name" {
			name = f.find("run -d")[i+1]
		}
	}
	if !strings.HasPrefix(name, LoginContainerPrefix) || len(name) != len(LoginContainerPrefix)+12 || s.name != name {
		t.Errorf("container name %q (session %q)", name, s.name)
	}
	for _, want := range []string{"--name " + name + " ", "--label rubi.role=login", "--network none", "--user 10001", "--cap-drop ALL", "--read-only",
		"--mount type=bind,source=" + real + ",target=" + real + " "} {
		if !strings.Contains(run+" ", want) {
			t.Errorf("login container lacks %q: %s", want, run)
		}
	}
	if strings.Count(run, "--mount") != 1 || strings.Contains(run, "target=/work") || strings.Contains(run, "readonly") || strings.Contains(run, "/rubi-auth") {
		t.Errorf("more is mounted than the sign-in folder, or it is read-only: %s", run)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("sign-in folder: %v %v", st, err)
	}
	for _, h := range []string{"claude.ai", "claude.com", "platform.claude.com", "console.anthropic.com", "api.anthropic.com"} {
		if !s.px.Allowed(h) {
			t.Errorf("%s not allowed", h)
		}
	}
	if s.px.Allowed("example.org") || s.px.Allowed("github.com") {
		t.Error("the proxy allows more than the sign-in hosts")
	}
	// its exec goes to the login container, and Close cleans that one up
	s.Close()
	if f.find("stop --time 2 "+name) == nil || f.find("delete --force "+name) == nil || f.find("stop --time 2 rubi-agent-auth") != nil {
		t.Errorf("cleanup: %v", f.cmds)
	}
	// two sign-ins get two names, and one that ends deletes only its own
	s2, err := a.StartLogin(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	s3, err := a.StartLogin(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.name == s3.name {
		t.Fatal("the same container name twice")
	}
	f.mu.Lock()
	f.cmds = nil
	f.mu.Unlock()
	s2.Close()
	if f.find("delete --force "+s2.name) == nil || f.find("delete --force "+s3.name) != nil || f.find("stop --time 2 "+s3.name) != nil {
		t.Errorf("closing one sign-in touched the other: %v", f.cmds)
	}
	s3.Close()
}

func TestSweepRemovesLeftoverSignInContainers(t *testing.T) {
	f := &authFake{ls: `[{"configuration":{"id":"rubi-t3-login-aaaaaaaaaaaa"}},{"configuration":{"id":"rubi-t3-login-bbbbbbbbbbbb"}},` +
		`{"configuration":{"id":"rubi-t3-r-abc-123"}},{"configuration":{"id":"buildkit"}},{"configuration":{"id":"rubi-agent-auth"}}]`}
	a, _ := newAuthVM(t, f)
	a.Sweep(context.Background())
	for _, want := range []string{"delete --force rubi-agent-auth", "delete --force rubi-t3-login-aaaaaaaaaaaa", "delete --force rubi-t3-login-bbbbbbbbbbbb"} {
		if f.find(want) == nil {
			t.Errorf("no %q: %v", want, f.cmds)
		}
	}
	// only sign-in containers: a session VM or the builder is not its business
	for _, not := range []string{"rubi-t3-r-abc-123", "buildkit"} {
		if f.find("delete --force "+not) != nil {
			t.Errorf("swept %s", not)
		}
	}
}

func TestSignInFolderChecks(t *testing.T) {
	base := t.TempDir()
	if _, err := EnsureSignInDir("relative/dir"); err == nil {
		t.Error("a relative folder")
	}
	if _, err := EnsureSignInDir(filepath.Join(base, "a,b")); err == nil {
		t.Error("a folder with a comma (it would add mount options)")
	}
	target := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "agent-signin")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureSignInDir(link); err == nil {
		t.Error("the folder is a link")
	}
	dir := filepath.Join(base, "ok")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	real, err := EnsureSignInDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", st.Mode())
	}
	if SignedInAt(real) {
		t.Error("signed in with no file")
	}
	cred := filepath.Join(real, CredentialFile)
	if err := os.WriteFile(cred, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if SignedInAt(real) {
		t.Error("signed in with an empty file")
	}
	if err := os.WriteFile(cred, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cred, 0o000); err != nil {
		t.Fatal(err)
	}
	if !SignedInAt(real) {
		t.Error("an unreadable credential file isn't seen: the check must only stat it")
	}
	_ = os.Chmod(cred, 0o600)
	env := LoginEnv(real)
	if env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] != real || env["CLAUDE_CONFIG_DIR"] != "/tmp/claude-login" || len(env) != 3 {
		t.Errorf("env %v", env)
	}
}

// Home's own folder, where the sign-in folder is, is never a project; neither is anything above or inside it.
func TestProjectsCantTouchTheSignInFolder(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "helper")
	signin := filepath.Join(helper, SignInDirName)
	os.MkdirAll(signin, 0o700)
	outside := filepath.Join(root, "my-project")
	os.MkdirAll(outside, 0o755)
	m := New(Options{Dir: helper, Projects: func() []string { return nil }})
	for name, dir := range map[string]string{"sign-in folder": signin, "helper dir": helper, "the parent": root} {
		real, _ := filepath.EvalSymlinks(dir)
		if !m.protected(real) {
			t.Errorf("%s is allowed as a project", name)
		}
	}
	real, _ := filepath.EvalSymlinks(outside)
	if m.protected(real) {
		t.Error("an ordinary folder is protected")
	}
}

// The sign-in container has the proxy for the sign-in hosts and nothing else: no listener answers for service 2 (the auth
// gateway of the earlier design), which stays reserved.
func TestSignInContainerHasNoGatewayAndTheProxyAllowsOnlySignInHosts(t *testing.T) {
	f := &authFake{}
	a, dir := newAuthVM(t, f)
	fastPipe(t)
	vm := newVMPipes(t, nil)
	a.spawn = vm.spawn
	s, err := a.StartLogin(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tok := strings.TrimPrefix(strings.TrimSuffix(s.env["HTTPS_PROXY"], "@127.0.0.1:3128"), "http://rubi:")
	if s.env["NO_PROXY"] != "127.0.0.1" || tok == "" {
		t.Errorf("proxy environment %v", s.env)
	}
	if got := connectVia(t, vm.latest(), netpipe.SvcProxy, "CONNECT example.org:443 HTTP/1.1\r\nHost: x\r\nProxy-Authorization: "+basic(tok)+"\r\n\r\n"); !strings.Contains(got, " 403 ") {
		t.Errorf("unlisted host through the pipe: %q", got)
	}
	for _, svc := range []byte{netpipe.SvcGateway, netpipe.SvcTestAPI, 77} {
		if got := connectVia(t, vm.latest(), svc, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); !strings.Contains(got, "reset") {
			t.Errorf("service %d is offered in the sign-in container: %q", svc, got)
		}
	}
	for _, h := range AuthHosts {
		if !s.px.Allowed(h) {
			t.Errorf("%s not allowed", h)
		}
	}
	for _, h := range []string{"example.org", "github.com", "registry.npmjs.org", "downloads.claude.ai"} {
		if s.px.Allowed(h) {
			t.Errorf("%s allowed", h)
		}
	}
	// the exec's environment file (it holds the proxy password) goes away shortly after the exec started
	old := envFileGrace
	envFileGrace = 50 * time.Millisecond
	defer func() { envFileGrace = old }()
	var envPath string
	a.spawn = newVMPipes(t, func(ctx context.Context, argv []string) (Conn, error) {
		for i, x := range argv {
			if x == "--env-file" {
				envPath = argv[i+1]
			}
		}
		return nopConn{}, nil
	}).spawn
	c, err := s.Exec(context.Background(), []string{LoginHelper}, map[string]string{"CLAUDE_CONFIG_DIR": "/tmp/claude"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := os.Stat(envPath); err != nil {
		t.Fatalf("env file missing at exec time: %v", err)
	}
	eventually(t, "env file removed", func() bool { _, err := os.Stat(envPath); return err != nil })
}
