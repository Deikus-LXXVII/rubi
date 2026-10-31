package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// authFake answers the container CLI for the auth volume; nothing real runs.
type authFake struct {
	cmds       [][]string
	volume     bool // the volume exists
	initFails  bool
	statusOut  string
	imageThere bool
}

func (f *authFake) run(ctx context.Context, argv []string) ([]byte, error) {
	f.cmds = append(f.cmds, argv)
	j := strings.Join(argv[1:], " ")
	switch {
	case strings.HasPrefix(j, "image inspect"):
		return nil, nil
	case strings.HasPrefix(j, "network inspect"):
		return os.ReadFile("testdata/container-1.6.1-network-internal.json")
	case strings.HasPrefix(j, "volume inspect"):
		if !f.volume {
			return nil, os.ErrNotExist
		}
		return nil, nil
	case strings.HasPrefix(j, "volume create"):
		f.volume = true
	case strings.HasPrefix(j, "volume delete"):
		f.volume = false
	case strings.Contains(j, "--cap-add CHOWN"):
		if f.initFails {
			return nil, errors.New("exit status 1")
		}
	case strings.Contains(j, "SIGNED_IN"):
		return []byte(f.statusOut), nil
	}
	return nil, nil
}

func (f *authFake) find(prefix string) []string {
	for _, c := range f.cmds {
		if strings.HasPrefix(strings.Join(c[1:], " "), prefix) {
			return c
		}
	}
	return nil
}

func newAuthVM(f *authFake) *AppleContainer {
	a := &AppleContainer{Bin: "container", EnvDir: os.TempDir()}
	a.run = f.run
	return a
}

func TestAuthVolumeIsCreatedAndPreparedOnce(t *testing.T) {
	f := &authFake{}
	a := newAuthVM(f)
	if err := a.EnsureAuthVolume(context.Background()); err != nil {
		t.Fatal(err)
	}
	init := strings.Join(f.find("run --rm --network rubi-agent --user 0"), " ")
	for _, want := range []string{"--cap-drop ALL", "--cap-add CHOWN", "--cap-add FOWNER", "type=volume,source=rubi-agent-auth,target=/rubi-auth",
		"mkdir -p /rubi-auth/token && chown 10001:10001 /rubi-auth/token && chmod 700 /rubi-auth/token"} {
		if !strings.Contains(init, want) {
			t.Errorf("init lacks %q: %s", want, init)
		}
	}
	n := len(f.cmds)
	if err := a.EnsureAuthVolume(context.Background()); err != nil || len(f.cmds) != n+1 { // one inspect
		t.Errorf("second call: %v, %d commands", err, len(f.cmds)-n)
	}
	// a failed preparation removes the volume, so the next call starts over
	f2 := &authFake{initFails: true}
	if err := newAuthVM(f2).EnsureAuthVolume(context.Background()); err == nil || f2.volume {
		t.Errorf("failed init: %v, volume %v", err, f2.volume)
	}
	// the volume name is configurable
	f3 := &authFake{}
	b := newAuthVM(f3)
	b.AuthVolume = "my-auth"
	b.EnsureAuthVolume(context.Background())
	if c := f3.find("volume create"); c[len(c)-1] != "my-auth" {
		t.Errorf("create: %v", c)
	}
}

// Every project's VM mounts the volume read-only at /rubi-auth, outside /work, and nothing of the
// sign-in is in the arguments; the per-project config directory is still the image's /work one.
func TestProjectVMsMountTheAuthVolumeReadOnly(t *testing.T) {
	a := &AppleContainer{Bin: "container"}
	for _, p := range []Project{{ID: "alpha", Dir: "/home/alice/alpha"}, {ID: "beta", Dir: "/home/alice/beta"}} {
		args := strings.Join(a.RunArgs(p), " ")
		if !strings.Contains(args, "--mount type=volume,source=rubi-agent-auth,target=/rubi-auth,readonly") {
			t.Errorf("%s: no read-only auth mount: %s", p.ID, args)
		}
		if !strings.Contains(args, "source="+p.Dir+",target=/work") || strings.Contains(args, p.Dir+"/rubi-auth") {
			t.Errorf("%s: %s", p.ID, args)
		}
		for _, bad := range []string{"oauth", "CLAUDE_CODE_OAUTH_TOKEN", "sk-ant", "CLAUDE_CONFIG_DIR"} {
			if strings.Contains(args, bad) {
				t.Errorf("%s: %q in the run arguments", p.ID, bad)
			}
		}
	}
	if got := a.RunArgs(Project{ID: "alpha", Dir: "/home/alice/alpha"}); strings.Contains(strings.Join(got, " "), "readonly,") {
		t.Error("odd mount option")
	}
}

func TestAuthStatusNeverReadsTheToken(t *testing.T) {
	f := &authFake{volume: true, statusOut: "SIGNED_IN\n{\"created\":\"2026-10-31T10:00:00Z\",\"expires\":\"2027-10-31T10:00:00Z\",\"method\":\"setup-token\"}\n"}
	a := newAuthVM(f)
	st, err := a.AuthStatus(context.Background())
	if err != nil || !st.SignedIn || st.Expires.Year() != 2027 || st.Created.Year() != 2026 {
		t.Fatalf("%+v %v", st, err)
	}
	probe := strings.Join(f.find("run --rm"), " ")
	if !strings.Contains(probe, "target=/rubi-auth,readonly") || !strings.Contains(probe, "--cap-drop ALL") || !strings.Contains(probe, "--read-only") {
		t.Errorf("probe container: %s", probe)
	}
	// the script tests that the file isn't empty and prints the metadata file only
	if !strings.Contains(statusScript, "[ -s /rubi-auth/token/oauth-token ]") || strings.Count(statusScript, "cat ") != 1 ||
		!strings.Contains(statusScript, "cat /rubi-auth/token/oauth-token.meta") {
		t.Errorf("the status script could read the token: %s", statusScript)
	}
	for _, c := range f.cmds {
		for _, x := range c {
			if strings.HasSuffix(x, "/oauth-token") {
				t.Errorf("the token path is an argument: %v", c)
			}
		}
	}
	if st.Expired(time.Date(2027, 11, 1, 0, 0, 0, 0, time.UTC)) != true || st.Expired(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("expiry")
	}
	for out, want := range map[string]bool{"NOT_SIGNED_IN\n": false, "SIGNED_IN\n": true} {
		f.statusOut = out
		if st, err := a.AuthStatus(context.Background()); err != nil || st.SignedIn != want || !st.Expires.IsZero() {
			t.Errorf("%q: %+v %v", out, st, err)
		}
	}
	f.statusOut = "garbage"
	if _, err := a.AuthStatus(context.Background()); err == nil {
		t.Error("garbage accepted")
	}
}

func TestAuthSignOutDeletesTokenAndMeta(t *testing.T) {
	f := &authFake{volume: true}
	if err := newAuthVM(f).AuthSignOut(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := f.find("run --rm")
	j := strings.Join(c, " ")
	if strings.Contains(j, "readonly") || !strings.Contains(j, "type=volume,source=rubi-agent-auth,target=/rubi-auth") ||
		!strings.Contains(j, "rm -f /rubi-auth/token/oauth-token /rubi-auth/token/oauth-token.meta") {
		t.Errorf("sign-out: %s", j)
	}
}

func TestStartAuthIsolatedAndProxyAllowsOnlySignInHosts(t *testing.T) {
	f := &authFake{volume: true}
	a := newAuthVM(f)
	a.spawn = func(ctx context.Context, argv []string) (Conn, error) { return nopConn{}, nil }
	a.authListen = "127.0.0.1:0"
	s, err := a.StartAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run := strings.Join(f.find("run -d"), " ")
	for _, want := range []string{"--name rubi-agent-auth", "--network rubi-agent", "--user 10001", "--cap-drop ALL", "--read-only",
		"--mount type=volume,source=rubi-agent-auth,target=/rubi-auth "} {
		if !strings.Contains(run+" ", want) {
			t.Errorf("auth container lacks %q: %s", want, run)
		}
	}
	if strings.Contains(run, "target=/work") || strings.Contains(run, "type=bind") {
		t.Errorf("a project is mounted in the auth container: %s", run)
	}
	// the proxy listens on the network's gateway and tunnels only to the sign-in hosts
	px := s.px
	for _, h := range AuthHosts {
		if !px.Allowed(h) {
			t.Errorf("%s not allowed", h)
		}
	}
	for _, h := range []string{"example.org", "github.com", "registry.npmjs.org", "downloads.claude.ai"} {
		if px.Allowed(h) {
			t.Errorf("%s allowed", h)
		}
	}
	// Exec passes the proxy through an env file, not an argument
	c, err := s.Exec(context.Background(), []string{AuthHelper}, map[string]string{"CLAUDE_CONFIG_DIR": "/tmp/claude"})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	s.Close()
	if f.find("stop --time 2 rubi-agent-auth") == nil || f.find("delete --force rubi-agent-auth") == nil {
		t.Errorf("not cleaned up: %v", f.cmds)
	}
}
