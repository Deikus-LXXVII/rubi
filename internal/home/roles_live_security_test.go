//go:build gatewaytest

package home

// Live check of the pre-release security fixes on real role VMs (M1: the gateway's request policy, M2/M3: the
// read-only .claude and .git/hooks). Built only with -tags gatewaytest (it needs the gateway's test hook):
//
//	RUBI_AGENT_LIVE=1 RUBI_AGENT_LIVE_RUN=rolesecurity go test -tags gatewaytest ./internal/home -run TestLiveRoleSecurity -v
//
// The gateway's upstream is a local TLS stand-in and the token a made-up one; nothing real is used and no model is
// called. Requests are sent from inside the role VM to its gateway port with the per-start secret from its own
// environment.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

func TestLiveRoleSecurity(t *testing.T) {
	if os.Getenv("RUBI_AGENT_LIVE") != "1" || os.Getenv("RUBI_AGENT_LIVE_RUN") != "rolesecurity" {
		t.Skip("set RUBI_AGENT_LIVE=1 RUBI_AGENT_LIVE_RUN=rolesecurity")
	}
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	const fake = "sk-ant-oat01-FAKEROLESECURITYMARK0123456789_abcdefghij-KLMNOP" // made up, never a credential
	tokDir := filepath.Join(agent.AuthDirOf(h.Dir), "token")
	if err := os.MkdirAll(tokDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(tokDir, "oauth-token")
	if err := os.WriteFile(tokenFile, []byte(fake+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tokenFile)

	var mu sync.Mutex
	var seen []http.Header
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"message","content":[]}`)
	}))
	defer upstream.Close()
	agent.SetTestGatewayUpstream(upstream.URL, upstream.Client())
	defer agent.SetTestGatewayUpstream("", nil)

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(base, "sec-live") // a git repository with no .claude
	if err := os.MkdirAll(filepath.Join(proj, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "sec-link") // its .claude is a symlink
	if err := os.MkdirAll(filepath.Join(base, "elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "elsewhere"), filepath.Join(link, ".claude")); err != nil {
		t.Fatal(err)
	}

	cfg := h.Fresh()
	mgr := agent.New(agent.Options{
		Dir: h.Dir, Projects: func() []string { return []string{proj, link} }, Wait: time.Minute,
		NewVM: func(agent.Project) agent.VM {
			return &agent.AppleContainer{RunnerDir: cfg.AgentRunnerDir, EnvDir: h.Dir}
		},
	})
	defer mgr.Close()
	put := func(project, name string, tools []string) string {
		skill := "---\nname: " + name + "-skill\ndescription: tiny\n---\nSay hi.\n"
		sum := sha256.Sum256([]byte(skill))
		sha := hex.EncodeToString(sum[:])
		args, _ := json.Marshal(map[string]any{"base_version": 0, "by": "live-test", "blobs": map[string]string{sha: skill},
			"spec": map[string]any{"project": project, "name": name, "description": "live " + name, "tools": tools,
				"skills": []map[string]any{{"name": name + "-skill", "source": "adhoc", "sha256": sha}}}})
		res, err := mgr.Handle(ctx, "claude-agent.role.put", args)
		if err != nil {
			t.Fatalf("role.put %s: %v", name, err)
		}
		b, _ := json.Marshal(res)
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(b, &out)
		return out.ID
	}
	plain := put("sec-live", "plain", []string{"Read"})
	search := put("sec-live", "search", []string{"Read", "WebSearch"})
	linkRole := put("sec-link", "linked", []string{"Read"})

	// M3: a symlinked .claude refuses the start.
	if _, err := mgr.SandboxRole(ctx, "sec-link", linkRole); err == nil {
		t.Errorf("M3: a role start with a symlinked .claude was accepted")
	} else {
		t.Logf("M3 symlinked .claude: start refused: %v", err)
	}

	first, err := os.ReadFile("agent/testdata/claude_code_first_turn.json")
	if err != nil {
		t.Fatal(err)
	}
	var tmpl map[string]any
	if err := json.Unmarshal(first, &tmpl); err != nil {
		t.Fatal(err)
	}
	variant := func(mod func(m map[string]any)) string {
		var m map[string]any
		_ = json.Unmarshal(first, &m)
		mod(m)
		b, _ := json.Marshal(m)
		return string(b)
	}
	type tcase struct {
		Name string `json:"name"`
		Body string `json:"body"`
		Beta string `json:"beta"`
	}
	cases := []tcase{
		{Name: "a-normal", Body: string(first), Beta: "claude-code-20250219"},
		{Name: "b-web_fetch", Body: variant(func(m map[string]any) {
			m["tools"] = []any{map[string]any{"type": "web_fetch_20250910", "name": "web_fetch"}}
		})},
		{Name: "c-mcp_servers", Body: variant(func(m map[string]any) {
			m["mcp_servers"] = []any{map[string]any{"type": "url", "url": "https://example.com/mcp", "name": "x"}}
		})},
		{Name: "d-image-url", Body: variant(func(m map[string]any) {
			m["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image",
				"source": map[string]any{"type": "url", "url": "https://example.com/x.png"}}}}}
		})},
		{Name: "e-beta", Body: string(first), Beta: "mcp-client-2025-04-04,claude-code-20250219"},
		{Name: "f-web_search", Body: variant(func(m map[string]any) {
			m["tools"] = []any{map[string]any{"type": "web_search_20250305", "name": "web_search"}}
		})},
	}
	in, _ := json.Marshal(cases)
	probe := `let s=""; process.stdin.on("data",d=>s+=d).on("end",async()=>{
const http=require("http"),u=new URL(process.env.ANTHROPIC_BASE_URL);
const go=(c)=>new Promise((res)=>{const h={"authorization":"Bearer "+process.env.CLAUDE_CODE_OAUTH_TOKEN,"x-api-key":"client-key","content-type":"application/json","anthropic-version":"2023-06-01"};
 if(c.beta)h["anthropic-beta"]=c.beta;
 const r=http.request({host:u.hostname,port:u.port,path:"/v1/messages?beta=true",method:"POST",headers:h},(x)=>{x.resume();x.on("end",()=>res(x.statusCode));});
 r.on("error",e=>res(e.code));r.end(c.body);});
const out={};for(const c of JSON.parse(s))out[c.name]=await go(c);
console.log("BASE="+u.host);console.log("RESULT="+JSON.stringify(out));});`

	runCases := func(sb *agent.Sandbox) (map[string]any, []http.Header) {
		t.Helper()
		mu.Lock()
		seen = nil
		mu.Unlock()
		c, err := sb.Exec(ctx, []string{"node", "-e", probe}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Write(in); err != nil {
			t.Fatal(err)
		}
		_ = c.CloseWrite()
		out, _ := io.ReadAll(io.LimitReader(c, 1<<20))
		t.Logf("probe output: %s", strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", " | "))
		var res map[string]any
		for _, l := range strings.Split(string(out), "\n") {
			if v, ok := strings.CutPrefix(l, "RESULT="); ok {
				_ = json.Unmarshal([]byte(v), &res)
			}
		}
		if res == nil {
			t.Fatalf("no result from the probe: %q", out)
		}
		mu.Lock()
		defer mu.Unlock()
		return res, append([]http.Header(nil), seen...)
	}

	// M1 on the role without WebSearch.
	sb, err := mgr.SandboxRole(ctx, "sec-live", plain)
	if err != nil {
		t.Fatalf("starting the VM of role %s: %v", plain, err)
	}
	res, hits := runCases(sb)
	want := map[string]float64{"a-normal": 200, "b-web_fetch": 403, "c-mcp_servers": 403, "d-image-url": 403, "e-beta": 200, "f-web_search": 403}
	for k, w := range want {
		if res[k] != w {
			t.Errorf("M1 %s: status %v, want %v", k, res[k], w)
		}
	}
	// Only a and e reach the stand-in, in this order (the probe runs the cases one after the other).
	if len(hits) != 2 {
		t.Fatalf("M1: %d requests reached the stand-in, want 2 (a and e)", len(hits))
	}
	a, e := hits[0], hits[1]
	if a.Get("Authorization") != "Bearer "+fake || a.Get("X-Api-Key") != "" {
		t.Errorf("M1a: upstream Authorization=%q x-api-key=%q", redactLive(a.Get("Authorization")), a.Get("X-Api-Key"))
	} else {
		t.Logf("M1a: upstream got the real-token header (matches the stand-in's token), x-api-key dropped; anthropic-beta=%q", a.Get("Anthropic-Beta"))
	}
	t.Logf("M1e: upstream anthropic-beta=%q", e.Get("Anthropic-Beta"))
	if b := e.Get("Anthropic-Beta"); strings.Contains(b, "mcp-client") || !strings.Contains(b, "claude-code-20250219") || !strings.Contains(b, "oauth-") {
		t.Errorf("M1e: anthropic-beta %q", b)
	}

	// M1f positive control: the role with WebSearch passes web_search.
	sb2, err := mgr.SandboxRole(ctx, "sec-live", search)
	if err != nil {
		t.Fatalf("starting the VM of role %s: %v", search, err)
	}
	res2, hits2 := runCases(sb2)
	if res2["f-web_search"] != float64(200) || res2["b-web_fetch"] != float64(403) {
		t.Errorf("M1f: role with WebSearch: f=%v (want 200) b=%v (want 403)", res2["f-web_search"], res2["b-web_fetch"])
	}
	t.Logf("M1f control: %d requests reached the stand-in from the WebSearch role", len(hits2))

	// M2/M3: /work/.claude and /work/.git/hooks are read-only in the role VM, though .claude did not exist before.
	run := func(sb *agent.Sandbox, script string) string {
		t.Helper()
		c, err := sb.Exec(ctx, []string{"sh", "-c", script}, nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.CloseWrite()
		out, _ := io.ReadAll(io.LimitReader(c, 1<<20))
		_ = c.Close()
		return string(out)
	}
	for _, sb := range []*agent.Sandbox{sb, sb2} {
		out := run(sb, "ls -ld /work/.claude /work/.git/hooks; "+
			"(mkdir /work/.claude/d 2>/dev/null && echo MKDIR-OK) || echo mkdir-denied; "+
			"(touch /work/.claude/f 2>/dev/null && echo TOUCH-OK) || echo touch-denied; "+
			"(echo x > /work/.claude/settings.json 2>/dev/null && echo SETTINGS-OK) || echo settings-denied; "+
			"(echo x > /work/.git/hooks/x 2>/dev/null && echo HOOK-OK) || echo hook-denied; "+
			"(touch /work/.git/hooks/pre-commit 2>/dev/null && echo HOOK2-OK) || echo hook2-denied; "+
			"(touch /work/.rubi/f 2>/dev/null && echo RUBI-OK) || echo rubi-denied; "+
			"(touch /work/ok-file && echo work-rw) || echo WORK-NOT-WRITABLE")
		t.Logf("M2/M3 in the VM:\n%s", out)
		for _, bad := range []string{"MKDIR-OK", "TOUCH-OK", "SETTINGS-OK", "HOOK-OK", "HOOK2-OK", "RUBI-OK", "WORK-NOT-WRITABLE"} {
			if strings.Contains(out, bad) {
				t.Errorf("M2/M3: %s", bad)
			}
		}
		for _, w := range []string{"mkdir-denied", "touch-denied", "settings-denied", "hook-denied", "hook2-denied", "work-rw"} {
			if !strings.Contains(out, w) {
				t.Errorf("M2/M3: missing %q", w)
			}
		}
	}
	// Nothing was written to the project through the shield.
	for _, p := range []string{".claude/d", ".claude/f", ".claude/settings.json", ".git/hooks/x", ".git/hooks/pre-commit"} {
		if _, err := os.Lstat(filepath.Join(proj, p)); err == nil {
			t.Errorf("M2/M3: %s exists in the project on this computer", p)
		}
	}
	if st, err := os.Lstat(filepath.Join(proj, ".claude")); err != nil || !st.IsDir() {
		t.Errorf("M2: .claude wasn't created as a directory at the start: %v", err)
	}
}
