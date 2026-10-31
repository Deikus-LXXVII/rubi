package agent

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Two project starts at once: each runner gets its own gateway and secret; the real token is in neither's
// environment, and only the gateway adds it, only toward the (fake) upstream.
func TestEachProjectStartGetsItsOwnGatewayAndSecret(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "helper")
	projs := map[string]string{"alpha": filepath.Join(root, "alpha"), "beta": filepath.Join(root, "beta")}
	for _, d := range []string{helper, projs["alpha"], projs["beta"], filepath.Join(helper, "agent-auth", "token")} {
		os.MkdirAll(d, 0o700)
	}
	os.WriteFile(filepath.Join(helper, "agent-auth", "token", "oauth-token"), []byte(fakeToken+"\n"), 0o600)
	up := newRecUpstream(t, nil)
	fakes := map[string]*Fake{"alpha": {}, "beta": {}}
	testGateway.upstream, testGateway.client = up.srv.URL, up.srv.Client()
	t.Cleanup(func() { testGateway.upstream, testGateway.client = "", nil })
	m := New(Options{Dir: helper, Wait: 5 * time.Second,
		Projects: func() []string { return []string{projs["alpha"], projs["beta"]} },
		NewVM:    func(p Project) VM { return fakes[p.ID] }})
	t.Cleanup(m.Close)
	for _, id := range []string{"alpha", "beta"} {
		if _, err := m.Sandbox(t.Context(), id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	env := map[string]map[string]string{"alpha": fakes["alpha"].Env(), "beta": fakes["beta"].Env()}
	secrets := map[string]bool{}
	for id, e := range env {
		secret := e["CLAUDE_CODE_OAUTH_TOKEN"]
		base := e["ANTHROPIC_BASE_URL"]
		if len(secret) < 32 || secret == fakeToken || secrets[secret] {
			t.Errorf("%s: secret %q", id, secret)
		}
		secrets[secret] = true
		u, err := url.Parse(base)
		if err != nil || u.Scheme != "http" || u.Host == "" || e["NO_PROXY"] != u.Hostname() || e["no_proxy"] != u.Hostname() {
			t.Errorf("%s: base %q no_proxy %q", id, base, e["NO_PROXY"])
		}
		if _, ok := e["ANTHROPIC_API_KEY"]; ok {
			t.Errorf("%s: an API key in the environment", id)
		}
		for k, v := range e {
			if strings.Contains(v, "FAKEFAKE") || strings.Contains(v, "sk-ant-") {
				t.Errorf("%s: the real token is in %s", id, k)
			}
		}
	}
	post := func(id, secret string) int {
		req, _ := http.NewRequest("POST", env[id]["ANTHROPIC_BASE_URL"]+"/v1/messages?beta=true", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if post("alpha", env["alpha"]["CLAUDE_CODE_OAUTH_TOKEN"]) != 200 || post("beta", env["beta"]["CLAUDE_CODE_OAUTH_TOKEN"]) != 200 {
		t.Error("a runner can't use its own gateway")
	}
	if post("alpha", env["beta"]["CLAUDE_CODE_OAUTH_TOKEN"]) != 401 || post("beta", env["alpha"]["CLAUDE_CODE_OAUTH_TOKEN"]) != 401 {
		t.Error("a secret works at another project's gateway")
	}
	if up.count() != 2 {
		t.Errorf("%d upstream requests", up.count())
	}
	for _, r := range up.reqs {
		if r.header.Get("Authorization") != "Bearer "+fakeToken {
			t.Errorf("the upstream got %q", r.header.Get("Authorization"))
		}
	}
	// the secret dies with the runner
	base := env["alpha"]["ANTHROPIC_BASE_URL"]
	fakes["alpha"].Runner().Exit()
	eventually(t, "alpha's gateway to close", func() bool {
		req, _ := http.NewRequest("POST", base+"/v1/messages", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+env["alpha"]["CLAUDE_CODE_OAUTH_TOKEN"])
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err != nil
	})
	if post("beta", env["beta"]["CLAUDE_CODE_OAUTH_TOKEN"]) != 200 {
		t.Error("beta's gateway stopped with alpha's runner")
	}
}

// The gateway sits on the address the VM reports for the Mac on its network, whatever the CONNECT proxy was
// given (agent_proxy_listen, agent_proxy_vm_host); the VM is told that address.
func TestGatewayAddressIsTheVMNetworksNotTheProxysSetting(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.ProxyListen = "127.0.0.1:0"
		o.ProxyVMHost = "mac.example.com"
	})
	e.fake.Gateway = "127.0.0.1"
	if _, err := e.m.Sandbox(e.ctx, "my-project"); err != nil {
		t.Fatal(err)
	}
	env := e.fake.Env()
	u, err := url.Parse(env["ANTHROPIC_BASE_URL"])
	if err != nil || u.Hostname() != "127.0.0.1" || env["NO_PROXY"] != "127.0.0.1" {
		t.Errorf("base %q no_proxy %q", env["ANTHROPIC_BASE_URL"], env["NO_PROXY"])
	}
	// the proxy keeps the host name it was given
	if !strings.Contains(env["HTTPS_PROXY"], "mac.example.com") {
		t.Errorf("proxy: %q", env["HTTPS_PROXY"])
	}
}
