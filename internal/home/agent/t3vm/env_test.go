package t3vm

import (
	"reflect"
	"strings"
	"testing"
)

// T3's environment as the S0 wrapper saw it, plus what a hostile or careless host could hold.
func hostileEnv() map[string]string {
	return map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-api03-secret", "ANTHROPIC_AUTH_TOKEN": "tok", "ANTHROPIC_BASE_URL": "http://evil.example",
		"CLAUDE_CODE_OAUTH_TOKEN": "oauth", "CLAUDE_CODE_USER_EMAIL": "e", "CLAUDE_CODE_USE_BEDROCK": "1", "CLAUDE_CODE_MESSAGING_SOCKET": "/s", "CLAUDE_CODE_SHELL_PREFIX": "/x", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR": "3", "CLAUDE_CODE_ACCOUNT_UUID": "u", "CLAUDE_CODE_ORGANIZATION_UUID": "o",
		"CLAUDE_CODE_MESSAGING_TOKEN": "m", "CLAUDE_CODE_SDK_HAS_HOST_AUTH_REFRESH": "1", "CLAUDE_CODE_OAUTH_SCOPES": "x",
		"CLAUDE_CODE_API_KEY_HELPER_TTL_MS": "1", "CLAUDE_AGENT_SDK_OAUTH_THING": "1", "CLAUDE_CODE_my_token": "lower",
		"SSH_AUTH_SOCK": "/tmp/agent.sock", "HOME": "/Users/someone", "PATH": "/usr/bin", "RANDOM_VAR": "x",
		"HTTPS_PROXY": "http://evil.example:1", "NO_PROXY": "*", "https_proxy": "http://evil.example:1", "CLAUDE_CONFIG_DIR": "/elsewhere",
		"CLAUDE_SECURESTORAGE_CONFIG_DIR": "/elsewhere2", "DISABLE_TELEMETRY": "0", "DISABLE_ERROR_REPORTING": "0",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "0", "CLAUDECODE": "1", "CLAUDE_CODEX": "1",
		"CLAUDE_CODE_NEWLINE": "a\nINJECTED=1", "CLAUDE_CODE_SIMPLE": "1",
		"CLAUDE_CODE_USE_VERTEX": "1", "AI_AGENT": "x", "API_TIMEOUT_MS": "1", "BAGGAGE": "b", "CLAUDE_CODE_REPORT_FINDINGS": "1",
		"CLAUDE_CODE_ANYTHING_NEW": "1", "CLAUDE_AGENT_SDK_ANYTHING": "1",
		// kept
		"CLAUDE_CODE_ENTRYPOINT": "sdk-ts", "CLAUDE_AGENT_SDK_VERSION": "1.2.3", "CLAUDE_CODE_EMIT_TOOL_USE_SUMMARIES": "1", "T3_AGENT_THREAD": "th-1",
	}
}

func TestFilterEnvKeepsOnlyTheAllowList(t *testing.T) {
	got := FilterEnv(hostileEnv())
	want := map[string]string{"CLAUDE_CODE_ENTRYPOINT": "sdk-ts", "CLAUDE_AGENT_SDK_VERSION": "1.2.3", "CLAUDE_CODE_EMIT_TOOL_USE_SUMMARIES": "1", "T3_AGENT_THREAD": "th-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestEnvLinesAreHomesOwnPlusTheFilteredEnv(t *testing.T) {
	s := testSpec(t)
	b, err := s.EnvLines(hostileEnv(), map[string]string{"HTTPS_PROXY": "http://rubi:pw@127.0.0.1:3128", "NO_PROXY": "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"CLAUDE_AGENT_SDK_VERSION=1.2.3",
		"CLAUDE_CODE_EMIT_TOOL_USE_SUMMARIES=1",
		"CLAUDE_CODE_ENTRYPOINT=sdk-ts",
		"CLAUDE_CONFIG_DIR=" + s.ClaudeDir,
		"CLAUDE_SECURESTORAGE_CONFIG_DIR=" + s.SignInDir,
		"HTTPS_PROXY=http://rubi:pw@127.0.0.1:3128",
		"NO_PROXY=127.0.0.1",
		"T3_AGENT_THREAD=th-1",
	}, "\n") + "\n"
	if string(b) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b, want)
	}
	// no proxy from Home means none at all: T3's own never gets through
	b, err = s.EnvLines(hostileEnv(), nil)
	if err != nil || strings.Contains(string(b), "PROXY") || strings.Contains(string(b), "evil") {
		t.Fatalf("T3's proxy reached the VM: %v\n%s", err, b)
	}
}

func TestEnvLinesRefuseOtherProxyVariables(t *testing.T) {
	if _, err := testSpec(t).EnvLines(nil, map[string]string{"ANTHROPIC_API_KEY": "k"}); err == nil {
		t.Fatal("an arbitrary variable passed as proxy")
	}
	s := testSpec(t)
	s.SignInDir = ""
	if _, err := s.EnvLines(nil, nil); err == nil {
		t.Fatal("an environment without a sign-in directory")
	}
}

// An unlisted CLAUDE_CODE_ name is dropped, and no listed name is one the secret filter would drop (the list and the
// filter must not drift apart).
func TestFilterEnvIsAListNotAPrefix(t *testing.T) {
	in := map[string]string{}
	for k := range allowedEnv {
		in[k] = "1"
	}
	if got := FilterEnv(in); len(got) != len(allowedEnv) {
		t.Fatalf("the filter drops listed names: %v", got)
	}
	for _, k := range []string{"CLAUDE_CODE_SIMPLE", "CLAUDE_CODE_SHELL_PREFIX", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_UNLISTED"} {
		if _, ok := FilterEnv(map[string]string{k: "1"})[k]; ok {
			t.Errorf("%s passed", k)
		}
	}
}
