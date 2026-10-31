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
}

func (f *authFake) run(ctx context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, argv)
	f.mu.Unlock()
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
	return a, filepath.Join(env, "agent-auth")
}

func TestAuthDirIsPrivateAndCreatedByHome(t *testing.T) {
	a, dir := newAuthVM(t, &authFake{})
	if err := a.EnsureAuthDir(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{dir, filepath.Join(dir, "token")} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v", d, st, err)
		}
	}
	// others' access is taken away again
	os.Chmod(dir, 0o755)
	if err := a.EnsureAuthDir(); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", st.Mode())
	}
	// a symlink in its place is refused
	b, dir2 := newAuthVM(t, &authFake{})
	target := t.TempDir()
	os.Symlink(target, dir2)
	if err := b.EnsureAuthDir(); err == nil {
		t.Error("a symlinked sign-in directory was accepted")
	}
	// a path that can't be mounted
	c := &AppleContainer{AuthDir: "/tmp/a,b"}
	if err := c.EnsureAuthDir(); err == nil {
		t.Error("a comma in the path was accepted")
	}
	// no directory configured: nothing to mount, nothing to create
	if (&AppleContainer{}).authMount(true) != "" || (&AppleContainer{}).EnsureAuthDir() != nil {
		t.Error("no EnvDir should mean no mount")
	}
}

// A project VM never has the sign-in directory, the token or anything of it: it talks to the auth gateway.
func TestProjectVMsNeverMountTheAuthDir(t *testing.T) {
	a := &AppleContainer{Bin: "container", AuthDir: "/home/alice/Rubi Home/agent-auth"}
	for _, p := range []Project{{ID: "alpha", Dir: "/home/alice/alpha"}, {ID: "beta", Dir: "/home/alice/beta"}} {
		args := strings.Join(a.RunArgs(p), " ")
		for _, bad := range []string{"agent-auth", "rubi-auth", "CLAUDE_CODE_OAUTH_TOKEN", "sk-ant", "oauth", "volume", "readonly"} {
			if strings.Contains(args, bad) {
				t.Errorf("%s: %q in the run arguments: %s", p.ID, bad, args)
			}
		}
		if !strings.Contains(args, "source="+p.Dir+",target=/work") || strings.Count(args, "--mount") != 1 {
			t.Errorf("%s: %s", p.ID, args)
		}
	}
}

func TestStartLeavesTheAuthDirAlone(t *testing.T) {
	f := &authFake{}
	a, dir := newAuthVM(t, f)
	a.spawn = newVMPipes(t, nil).spawn
	fastPipe(t)
	if _, err := a.Start(context.Background(), StartSpec{Project: Project{ID: "my-project", Dir: "/home/alice/my-project"}, Pipe: &PipeSpec{Proxy: netpipe.NewListener("p", 1)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("starting a project VM created the sign-in directory")
	}
	if f.find("volume") != nil {
		t.Error("a volume command ran")
	}
}

func TestAuthStatusStatsTheTokenAndReadsOnlyTheMeta(t *testing.T) {
	a, dir := newAuthVM(t, &authFake{})
	if err := a.EnsureAuthDir(); err != nil {
		t.Fatal(err)
	}
	st, err := a.AuthStatus(context.Background())
	if err != nil || st.SignedIn {
		t.Fatalf("empty: %+v %v", st, err)
	}
	tok := filepath.Join(dir, "token", "oauth-token")
	os.WriteFile(tok, nil, 0o600) // empty is not signed in
	if st, _ := a.AuthStatus(context.Background()); st.SignedIn {
		t.Error("an empty token file counts")
	}
	os.WriteFile(tok, []byte("sk-ant-oat01-FAKEFAKEFAKEFAKEFAKEFAKE\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "token", "oauth-token.meta"),
		[]byte(`{"created":"2026-10-31T10:00:00Z","expires":"2027-10-31T10:00:00Z","method":"setup-token"}`), 0o600)
	st, err = a.AuthStatus(context.Background())
	if err != nil || !st.SignedIn || st.Expires.Year() != 2027 || st.Created.Year() != 2026 {
		t.Fatalf("%+v %v", st, err)
	}
	if !st.Expired(time.Date(2027, 11, 1, 0, 0, 0, 0, time.UTC)) || st.Expired(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("expiry")
	}
	// no meta: signed in, expiry unknown; a garbage meta likewise; a symlink for the token isn't a token
	os.WriteFile(filepath.Join(dir, "token", "oauth-token.meta"), []byte("garbage"), 0o600)
	if st, _ := a.AuthStatus(context.Background()); !st.SignedIn || !st.Expires.IsZero() {
		t.Errorf("garbage meta: %+v", st)
	}
	os.Remove(tok)
	os.Symlink("/etc/hosts", tok)
	if st, _ := a.AuthStatus(context.Background()); st.SignedIn {
		t.Error("a symlink counts as a token")
	}
	// the status code never opens the token file
	src, _ := os.ReadFile("auth.go")
	body := string(src)
	i := strings.Index(body, "func AuthStatusOf")
	j := strings.Index(body, "// AuthStatus reports")
	if strings.Contains(body[i:j], "ReadFile") || strings.Contains(body[i:j], "Open(filepath.Join(dir, \"token\", tokenFile") {
		t.Error("AuthStatusOf reads the token file")
	}
}

func TestAuthSignOutRemovesBothFiles(t *testing.T) {
	a, dir := newAuthVM(t, &authFake{})
	a.EnsureAuthDir()
	for _, f := range []string{"oauth-token", "oauth-token.meta"} {
		os.WriteFile(filepath.Join(dir, "token", f), []byte("x"), 0o600)
	}
	if err := a.AuthSignOut(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "token")); len(ents) != 0 {
		t.Errorf("left: %v", ents)
	}
	if err := a.AuthSignOut(context.Background()); err != nil {
		t.Errorf("signing out twice: %v", err)
	}
}

func TestStartAuthIsolatedAndProxyAllowsOnlySignInHosts(t *testing.T) {
	f := &authFake{}
	a, dir := newAuthVM(t, f)
	fastPipe(t)
	vm := newVMPipes(t, nil)
	a.spawn = vm.spawn
	s, err := a.StartAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run := strings.Join(f.find("run -d"), " ")
	for _, want := range []string{"--name rubi-agent-auth", "--label rubi.role=auth", "--network none", "--user 10001", "--cap-drop ALL", "--read-only",
		"--mount type=bind,source=" + dir + ",target=/rubi-auth "} {
		if !strings.Contains(run+" ", want) {
			t.Errorf("auth container lacks %q: %s", want, run)
		}
	}
	if strings.Contains(run, "target=/work") || strings.Contains(run, "readonly") {
		t.Errorf("a project is mounted, or the directory is read-only: %s", run)
	}
	// the sign-in's proxy is reached through the pipe, with its own per-start password
	if s.env["HTTPS_PROXY"] == "" || !strings.HasSuffix(s.env["HTTPS_PROXY"], "@127.0.0.1:3128") || s.env["NO_PROXY"] != "127.0.0.1" {
		t.Errorf("proxy environment %v", s.env)
	}
	tok := strings.TrimPrefix(strings.TrimSuffix(s.env["HTTPS_PROXY"], "@127.0.0.1:3128"), "http://rubi:")
	if got := connectVia(t, vm.latest(), netpipe.SvcProxy, "CONNECT example.org:443 HTTP/1.1\r\nHost: x\r\nProxy-Authorization: "+basic(tok)+"\r\n\r\n"); !strings.Contains(got, " 403 ") {
		t.Errorf("unlisted host through the pipe: %q", got)
	}
	if got := connectVia(t, vm.latest(), netpipe.SvcGateway, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); !strings.Contains(got, "reset") {
		t.Errorf("the sign-in has no auth gateway: %q", got)
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
	c, err := s.Exec(context.Background(), []string{AuthHelper}, map[string]string{"CLAUDE_CONFIG_DIR": "/tmp/claude"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := os.Stat(envPath); err != nil {
		t.Fatalf("env file missing at exec time: %v", err)
	}
	eventually(t, "env file removed", func() bool { _, err := os.Stat(envPath); return err != nil })
	s.Close()
	if f.find("stop --time 2 rubi-agent-auth") == nil || f.find("delete --force rubi-agent-auth") == nil {
		t.Errorf("not cleaned up: %v", f.cmds)
	}
}

func TestSweepRemovesALeftoverAuthContainer(t *testing.T) {
	f := &authFake{}
	a, _ := newAuthVM(t, f)
	a.SweepAuth(context.Background())
	if c := f.find("delete --force rubi-agent-auth"); c == nil {
		t.Errorf("no sweep: %v", f.cmds)
	}
}

// No project can be, contain or sit inside the helper's directory, where the sign-in directory is.
func TestProjectsCantTouchTheAuthDir(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "helper")
	auth := filepath.Join(helper, "agent-auth")
	os.MkdirAll(filepath.Join(auth, "token"), 0o700)
	outside := filepath.Join(root, "my-project")
	os.MkdirAll(outside, 0o755)
	m := New(Options{Dir: helper, Projects: func() []string { return nil }})
	for name, dir := range map[string]string{"auth dir": auth, "inside it": filepath.Join(auth, "token"), "helper dir": helper, "the parent": root} {
		real, _ := filepath.EvalSymlinks(dir)
		if !m.protected(real) {
			t.Errorf("%s is allowed as a project", name)
		}
	}
	real, _ := filepath.EvalSymlinks(outside)
	if m.protected(real) {
		t.Error("an ordinary folder is protected")
	}
	// a custom directory elsewhere is protected the same way
	custom := filepath.Join(root, "elsewhere", "auth")
	os.MkdirAll(custom, 0o700)
	m2 := New(Options{Dir: helper, AuthDir: custom})
	rc, _ := filepath.EvalSymlinks(custom)
	if !m2.protected(rc) || !m2.protected(filepath.Dir(rc)) {
		t.Error("a custom sign-in directory can be a project")
	}
}
