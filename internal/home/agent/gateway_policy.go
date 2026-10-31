package agent

// What the auth gateway lets through to the model API. The gateway is the only path from the VM to Anthropic, and
// Anthropic runs some tools on its own servers (web fetch, code execution, the MCP connector): a request that
// enables one is a way to reach hosts the CONNECT proxy's allow-list would refuse. The body of a request is
// therefore checked before it is forwarded, and only tools that run on the client (or only look at the request's
// own tool list) are let through. Anything not on the list is refused, so a server tool Anthropic adds later is
// refused until it is added here on purpose.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
)

var (
	// clientToolType are the typed tools that the client executes (Claude Code runs them itself; the model only
	// emits a tool_use): versioned bash, text editor, computer use and memory tools. A tool without a type, or of
	// type "custom", is a plain client tool and is always fine.
	clientToolType = regexp.MustCompile(`^(bash|text_editor|computer|memory)_[0-9]{8}$`)
	// toolSearchType is Anthropic's tool search: it looks only at the tools of the request itself.
	toolSearchType = regexp.MustCompile(`^tool_search_tool_[a-z_]+_[0-9]{8}$`)
	// advisorType is the advisor tool: the server asks another model; it has no network access of its own.
	advisorType = regexp.MustCompile(`^advisor_[0-9]{8}$`)
	// webSearchType is the server-side web search. It is allowed only when the session may use WebSearch.
	webSearchType = regexp.MustCompile(`^web_search_[0-9]{8}$`)
)

// betaPrefixes are the anthropic-beta values that are passed on (a value matches when it starts with one; the
// dated suffix changes from release to release). They are the names found in the bundled Claude Code binary, less
// the ones that switch on a feature that reaches outside (or that Rubi doesn't use): mcp-servers-*, mcp-tunnels-*,
// mcp-client-*, files-api-*, code-execution-*, web-fetch-*, managed-agents-*, environments-*, ccr-*,
// oidc-federation-*, message-batches-*, user-profiles-*, bedrock-*, vertex-*, and dangerous-tool-use-* (what it
// unlocks is not documented; it is not passed on until it is known). Dropping a beta whose body field is still sent
// can make the API answer 400, which is why the list is the CLI's whole set and not a short one.
// web-search-* is kept only when the session may use WebSearch (betaWebSearch). advisor-tool-* is kept: the
// advisor tool asks another model on the server and has no network access of its own.
var betaPrefixes = []string{
	"claude-code-", "oauth-", "advanced-tool-use-", "advisor-tool-", "afk-mode-", "agent-memory-", "auto-mode-classifier-",
	"cache-diagnosis-", "cache-keepalive-", "context-1m-", "context-hint-", "context-management-", "dreaming-",
	"effort-", "extended-cache-ttl-", "fallback-credit-", "fast-mode-", "fine-grained-tool-streaming-", "inline-tools-",
	"interleaved-thinking-", "memory-", "message-threads-", "mid-conversation-system-", "mid-conversation-tool-changes-",
	"output-128k-", "per-turn-control-", "prompt-caching-", "redact-thinking-", "server-side-fallback-",
	"structured-outputs-", "task-budgets-", "thinking-binding-controls-", "thinking-display-updates-",
	"thinking-resumption-", "thinking-token-count-", "timing-", "token-counting-", "token-efficient-tools-",
	"tool-search-tool-",
}

const betaWebSearch = "web-search-"

// filterBeta keeps the allowed values of the anthropic-beta lines, once each, and adds the OAuth beta.
func filterBeta(lines []string, allowWebSearch bool) string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, l := range lines {
		for _, v := range strings.Split(l, ",") {
			v = strings.TrimSpace(v)
			if len(v) >= 100 || strings.ContainsAny(v, " \t\r\n") {
				continue
			}
			if allowWebSearch && strings.HasPrefix(v, betaWebSearch) {
				add(v)
				continue
			}
			for _, p := range betaPrefixes {
				if strings.HasPrefix(v, p) {
					add(v)
					break
				}
			}
		}
	}
	add(oauthBeta)
	return strings.Join(out, ",")
}

// strictObject splits a JSON object into its members. An object with a repeated key (or keys that differ only
// in case) is refused: another parser could read it differently from this one.
func strictObject(raw []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not an object")
	}
	out := map[string]json.RawMessage{}
	folded := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := kt.(string)
		if folded[strings.ToLower(k)] {
			return nil, errors.New("repeated key")
		}
		folded[strings.ToLower(k)] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return out, nil
}

// checkBody decides whether a request body may be forwarded. It returns "" or the reason it is refused.
func checkBody(body []byte, allowWebSearch bool) (reason string) {
	top, err := strictObject(body)
	if err != nil {
		return "the body isn't a JSON object that can be checked"
	}
	for k := range top {
		switch strings.ToLower(k) {
		case "mcp_servers", "container":
			return "'" + clip(k, 20) + "' (a server-side feature) isn't allowed" // in any case: a parser may fold it
		case "tools":
			if k != "tools" {
				return "a field can't be checked"
			}
		}
	}
	raw, ok := top["tools"]
	if !ok || string(bytes.TrimSpace(raw)) == "null" {
		return ""
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return "'tools' isn't a list"
	}
	for _, t := range tools {
		o, err := strictObject(t)
		if err != nil {
			return "a tool can't be checked"
		}
		for k := range o {
			if strings.EqualFold(k, "type") && k != "type" {
				return "a tool has a type that can't be checked"
			}
		}
		tr, has := o["type"]
		if !has {
			continue
		}
		var typ *string
		if err := json.Unmarshal(tr, &typ); err != nil {
			return "a tool has a type that can't be checked"
		}
		if typ == nil || *typ == "custom" || clientToolType.MatchString(*typ) || toolSearchType.MatchString(*typ) || advisorType.MatchString(*typ) {
			continue
		}
		if webSearchType.MatchString(*typ) && allowWebSearch {
			continue
		}
		return "the server-side tool type '" + clip(*typ, 40) + "' isn't allowed"
	}
	return ""
}
