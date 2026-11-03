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

// A VM without a network is served through its pipe: the runner is told the loopback address of the forwarder, the
// proxy listens on no socket and does its own checks on the streams. It gets no credential, no API address and no
// service for one: service 2 (the auth gateway of the earlier design) is refused.
func TestNetlessVMGetsLoopbackAddressesAndStreamServices(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "helper")
	proj := filepath.Join(root, "alpha")
	for _, d := range []string{helper, proj} {
		os.MkdirAll(d, 0o700)
	}
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
	tok := strings.TrimSuffix(strings.TrimPrefix(env["HTTPS_PROXY"], "http://rubi:"), "@127.0.0.1:3128")
	if env["HTTPS_PROXY"] != "http://rubi:"+tok+"@127.0.0.1:3128" || env["https_proxy"] != env["HTTPS_PROXY"] || len(tok) < 32 ||
		env["NO_PROXY"] != "127.0.0.1" || env["no_proxy"] != "127.0.0.1" || strings.Contains(env["HTTPS_PROXY"], env["RUBI_RUN_TOKEN"]) {
		t.Fatalf("env: %v", env)
	}
	// no credential and no API address of any kind is handed to the runner
	for k, v := range env {
		if strings.Contains(k, "TOKEN") && k != "RUBI_RUN_TOKEN" || strings.Contains(k, "ANTHROPIC") || strings.Contains(v, "sk-ant-") {
			t.Errorf("the runner's environment has %s", k)
		}
	}
	vm := fake.PipeMux()
	// nothing listens on the Mac: the proxy is on a listener that is not a socket
	m.mu.Lock()
	px := m.projs["alpha"].proxy
	m.mu.Unlock()
	if px.Addr() != "netpipe:proxy" {
		t.Fatalf("address %q", px.Addr())
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
	// no service answers for the auth gateway (2) or the test API (5): a stream to them is reset
	for _, svc := range []byte{netpipe.SvcGateway, netpipe.SvcTestAPI} {
		if got := connectVia(t, vm, svc, "POST /v1/messages HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\n{}"); !strings.Contains(got, "reset") {
			t.Errorf("service %d answered: %q", svc, got)
		}
	}
	// the proxy goes with the runner
	fake.Runner().Exit()
	eventually(t, "services closed", func() bool {
		s, err := vm.Open(netpipe.SvcProxy)
		if err != nil {
			return true
		}
		s.SetReadDeadline(time.Now().Add(time.Second))
		_, err = s.Read(make([]byte, 1))
		return errors.Is(err, netpipe.ErrReset) || errors.Is(err, netpipe.ErrChannelClosed)
	})
}
