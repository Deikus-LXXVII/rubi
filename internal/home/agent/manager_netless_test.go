package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// A VM without a network is served through its pipe: the runner is told the loopback addresses of the forwarder,
// the proxy and the gateway listen on no socket, and each does its own checks on the streams.
func TestNetlessVMGetsLoopbackAddressesAndStreamServices(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "helper")
	proj := filepath.Join(root, "alpha")
	for _, d := range []string{helper, proj, filepath.Join(helper, "agent-auth", "token")} {
		os.MkdirAll(d, 0o700)
	}
	os.WriteFile(filepath.Join(helper, "agent-auth", "token", "oauth-token"), []byte(fakeToken+"\n"), 0o600)
	up := newRecUpstream(t, nil)
	testGateway.upstream, testGateway.client = up.srv.URL, up.srv.Client()
	t.Cleanup(func() { testGateway.upstream, testGateway.client = "", nil })
	fake := &Fake{NoNetwork: true}
	ups := newUpstream(t)
	m := New(Options{Dir: helper, Wait: 5 * time.Second,
		Projects: func() []string { return []string{proj} },
		// whatever the old settings say, a VM without a network doesn't use them
		ProxyListen: "127.0.0.1:0", ProxyVMHost: "mac.example.com",
		Resolve: publicResolve, Dial: ups.dial,
		NewVM: func(p Project) VM { return fake }})
	t.Cleanup(m.Close)
	if _, err := m.Sandbox(t.Context(), "alpha"); err != nil {
		t.Fatal(err)
	}
	env := fake.Env()
	secret := env["CLAUDE_CODE_OAUTH_TOKEN"]
	tok := strings.TrimSuffix(strings.TrimPrefix(env["HTTPS_PROXY"], "http://rubi:"), "@127.0.0.1:3128")
	if env["HTTPS_PROXY"] != "http://rubi:"+tok+"@127.0.0.1:3128" || env["https_proxy"] != env["HTTPS_PROXY"] || len(tok) < 32 ||
		env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:3129" || env["NO_PROXY"] != "127.0.0.1" || env["no_proxy"] != "127.0.0.1" ||
		len(secret) < 32 || secret == fakeToken || strings.Contains(env["HTTPS_PROXY"], env["RUBI_RUN_TOKEN"]) {
		t.Fatalf("env: %v", env)
	}
	for k, v := range env {
		if strings.Contains(v, "FAKEFAKE") || strings.Contains(v, "sk-ant-") {
			t.Errorf("the real token is in %s", k)
		}
	}
	vm := fake.PipeMux()
	// nothing listens on the Mac: the proxy and the gateway are on listeners that are not sockets
	m.mu.Lock()
	px, gw := m.projs["alpha"].proxy, m.projs["alpha"].gateway
	m.mu.Unlock()
	if px.Addr() != "netpipe:proxy" || gw.Addr() != "netpipe:gateway" {
		t.Fatalf("addresses %q %q", px.Addr(), gw.Addr())
	}
	// the proxy's checks are unchanged: credentials, the allow-list, the port
	req := func(host, auth string) string {
		r := "CONNECT " + host + " HTTP/1.1\r\nHost: " + host + "\r\n"
		if auth != "" {
			r += "Proxy-Authorization: " + auth + "\r\n"
		}
		return connectVia(t, vm, netpipe.SvcProxy, r+"\r\n")
	}
	if got := req("registry.npmjs.org:443", ""); !strings.Contains(got, " 407 ") {
		t.Errorf("no credentials: %q", got)
	}
	if got := req("registry.npmjs.org:443", basic("wrong-wrong-wrong-wrong")); !strings.Contains(got, " 407 ") {
		t.Errorf("wrong credentials: %q", got)
	}
	if got := req("example.org:443", basic(tok)); !strings.Contains(got, " 403 ") {
		t.Errorf("unlisted host: %q", got)
	}
	if got := req("registry.npmjs.org:80", basic(tok)); !strings.Contains(got, " 403 ") {
		t.Errorf("port 80: %q", got)
	}
	if got := req("127.0.0.1:443", basic(tok)); !strings.Contains(got, " 403 ") {
		t.Errorf("an address literal: %q", got)
	}
	// the gateway's checks are unchanged: the secret, the path, then the token only goes upstream
	post := func(path, bearer string) string {
		return connectVia(t, vm, netpipe.SvcGateway, "POST "+path+" HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer "+bearer+"\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
	}
	if got := post("/v1/messages", "wrong"); !strings.Contains(got, " 401 ") {
		t.Errorf("wrong secret: %q", got)
	}
	if got := post("/v1/other", secret); !strings.Contains(got, " 404 ") {
		t.Errorf("other path: %q", got)
	}
	if got := post("/v1/messages?beta=true", secret); !strings.Contains(got, " 200 ") {
		t.Errorf("good request: %q", got)
	}
	if up.count() != 1 || up.reqs[0].header.Get("Authorization") != "Bearer "+fakeToken {
		t.Errorf("upstream: %d requests", up.count())
	}
	// the secret dies with the runner: afterwards the services are gone
	fake.Runner().Exit()
	eventually(t, "services closed", func() bool {
		s, err := vm.Open(netpipe.SvcGateway)
		if err != nil {
			return true
		}
		s.SetReadDeadline(time.Now().Add(time.Second))
		_, err = s.Read(make([]byte, 1))
		return errors.Is(err, netpipe.ErrReset) || errors.Is(err, netpipe.ErrChannelClosed)
	})
}
