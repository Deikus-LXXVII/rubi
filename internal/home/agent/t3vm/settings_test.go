package t3vm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestManagedSettingsHaveOnlyTheManagedHookAndDenyTheSignIn(t *testing.T) {
	b, err := ManagedSettings("/h/agent-signin")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		AllowManagedHooksOnly bool `json:"allowManagedHooksOnly"`
		Hooks                 map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if !v.AllowManagedHooksOnly {
		t.Error("allowManagedHooksOnly is off: a project's hooks would run")
	}
	pre := v.Hooks["PreToolUse"]
	if len(v.Hooks) != 1 || len(pre) != 1 || pre[0].Matcher != "*" || len(pre[0].Hooks) != 1 {
		t.Fatalf("hooks: %s", b)
	}
	h := pre[0].Hooks[0]
	if h.Type != "command" || h.Command != "node /etc/rubi/rubi-hook.mjs" || h.Timeout != 300 {
		t.Errorf("hook: %+v", h)
	}
	deny := strings.Join(v.Permissions.Deny, "\n")
	for _, want := range []string{"Read(//h/agent-signin/**)", "Edit(//h/agent-signin/**)", "Bash(*/h/agent-signin*)"} {
		if !strings.Contains(deny, want) {
			t.Errorf("no deny rule %s in:\n%s", want, deny)
		}
	}
}

func TestManagedSettingsRefusePathsThatAreRulePatterns(t *testing.T) {
	for _, p := range []string{"", "relative", "/h/a*b", "/h/a(b)", "/h/a,b"} {
		if _, err := ManagedSettings(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestHookTimeoutsNestInsideTheHookTimeout(t *testing.T) {
	// the script (295 s), the gate (285 s) and the manager's wait (280 s) all end before the hook's 300 s
	if !strings.Contains(string(HookScript), "295000") || DefaultGateWait.Seconds() >= HookTimeoutSeconds || DefaultGateWait.Seconds() < 280 {
		t.Fatalf("timeouts: gate %s, hook %d", DefaultGateWait, HookTimeoutSeconds)
	}
}
