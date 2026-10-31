package agent

import (
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
	"net/http"
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
