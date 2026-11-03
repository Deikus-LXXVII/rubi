package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/plugins"
)

const t3Plugin = "t3agents" // plugins.OfficialT3PluginID

// The T3 families are the claude ones under other names: the same gate answers for them.
func TestHomeOpMapsT3OpsToTheClaudeOnes(t *testing.T) {
	for in, want := range map[string]string{
		"t3-agent.role.put": opRolePut, "t3-agent.ssh.register": opSSHRegister, "t3-agent.start": "claude-agent.start",
		"t3-setup.project.add": "agent-setup.project.add", "t3-setup.run": "agent-setup.run",
		"claude-agent.role.put": opRolePut, "hue.request": "hue.request", "t3-agentx.role.put": "t3-agentx.role.put",
	} {
		if got := homeOp(in); got != want {
			t.Errorf("homeOp(%q) = %q, want %q", in, got, want)
		}
	}
	if plugins.OfficialT3PluginID != t3Plugin {
		t.Fatalf("the test assumes the T3 plugin is %q", t3Plugin)
	}
}

const t3PutOp = "t3-agent.role.put"

// An unattested t3-agent.role.put is refused like the claude one; a real approval of the plugin's own kind passes.
func TestHomeGateT3RolePutApprovals(t *testing.T) {
	c := targetCore(t)
	if err := attest(c, t3Plugin, t3PutOp, putArgs("apr_made_up", "")); err == nil {
		t.Error("a fabricated id was accepted for t3-agent.role.put")
	}
	decide(t, c, t3Plugin, "t3agents.role_widen", nil, func(id string) {
		if err := attest(c, t3Plugin, t3PutOp, putArgs(id, id)); err != nil {
			t.Errorf("a real approval was refused: %v", err)
		}
		if err := attest(c, t3Plugin, t3PutOp, putArgs("", "")); err != nil {
			t.Errorf("a put without approvals was refused: %v", err)
		}
		if err := attest(c, t3Plugin, t3PutOp, putArgs("apr_made_up", id)); err == nil {
			t.Error("a fabricated id was accepted next to a real one")
		}
		if err := attest(c, "claude-code", opRolePut, putArgs(id, id)); err == nil {
			t.Error("the claude-code plugin used the T3 plugin's approval")
		}
		if err := attest(c, "other-plugin", t3PutOp, putArgs(id, id)); err == nil {
			t.Error("another plugin used the T3 plugin's approval")
		}
		// Home is told the claude-code spelling of the kind, whatever the plugin's namespace.
		out, err := c.attestHomeCall(t3Plugin, t3PutOp, putArgs(id, id))
		if err != nil {
			t.Fatal(err)
		}
		var o struct {
			K map[string]string `json:"attested_kinds"`
		}
		if err := json.Unmarshal(out, &o); err != nil || o.K[id] != "claude-code.role_widen" {
			t.Errorf("attested_kinds = %v (%v)", o.K, err)
		}
	})
	// An approval of the claude-code namespace is not the T3 plugin's.
	decide(t, c, t3Plugin, "claude-code.role_widen", nil, func(id string) {
		if err := attest(c, t3Plugin, t3PutOp, putArgs(id, "")); err == nil {
			t.Error("an approval of another plugin's kind was accepted")
		}
	})
	decide(t, c, t3Plugin, "t3agents.fork", nil, func(id string) {
		if err := attest(c, t3Plugin, t3PutOp, putArgs(id, "")); err == nil {
			t.Error("an approval of a kind that is not for roles was accepted")
		}
	})
	if err := attest(c, t3Plugin, "t3-agent.role.list", json.RawMessage(`{}`)); err != nil {
		t.Errorf("a read was gated: %v", err)
	}
}

// The skill-accept approval of the plugin being executed is attested as the claude-code kind (Home limits it to
// skill additions by that name).
func TestHomeGateT3SkillAcceptIsCanonical(t *testing.T) {
	c := targetCore(t)
	decide(t, c, t3Plugin, "t3agents.role_skill_accept", nil, func(id string) {
		out, err := c.attestHomeCall(t3Plugin, t3PutOp, json.RawMessage(`{"id":"r","spec":{},"change":{"approval":"`+id+`"}}`))
		if err != nil {
			t.Fatal(err)
		}
		var o struct {
			K map[string]string `json:"attested_kinds"`
		}
		_ = json.Unmarshal(out, &o)
		if o.K[id] != "claude-code.role_skill_accept" {
			t.Errorf("attested_kinds = %v", o.K)
		}
	})
	// The id the plugin makes itself for a skill acceptance that is being executed.
	decide(t, c, t3Plugin, "t3agents.role_skill_accept", json.RawMessage(`{"approval_id":"sk_1"}`), func(string) {
		out, err := c.attestHomeCall(t3Plugin, t3PutOp, json.RawMessage(`{"id":"r","spec":{},"change":{"approval":"sk_1"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `"sk_1":"claude-code.role_skill_accept"`) {
			t.Errorf("out = %s", out)
		}
		if err := attest(c, "claude-code", opRolePut, json.RawMessage(`{"id":"r","spec":{},"change":{"approval":"sk_1"}}`)); err == nil {
			t.Error("claude-code used the T3 plugin's pending skill acceptance")
		}
	})
	if err := attest(c, t3Plugin, t3PutOp, json.RawMessage(`{"id":"r","spec":{},"change":{"approval":"sk_1"}}`)); err == nil {
		t.Error("the skill acceptance outlived its approval")
	}
}

const t3SSHOp = "t3-agent.ssh.register"

func TestHomeGateT3SSHRegister(t *testing.T) {
	c := targetCore(t)
	if err := attest(c, t3Plugin, t3SSHOp, json.RawMessage(sshArgs)); err == nil {
		t.Fatal("registered with no approval")
	}
	payload := json.RawMessage(`{"device":"d","name":"web","host":"example.com","port":22,"user":"deploy","fingerprint":"SHA256:abc","ask_each_time":true}`)
	decide(t, c, t3Plugin, "t3agents.ssh_target", payload, func(string) {
		if err := attest(c, t3Plugin, t3SSHOp, json.RawMessage(sshArgs)); err != nil {
			t.Errorf("the approved target was refused: %v", err)
		}
		if err := attest(c, t3Plugin, t3SSHOp, json.RawMessage(strings.Replace(sshArgs, "example.com", "evil.example.net", 1))); err == nil {
			t.Error("a different host than the one approved was accepted")
		}
		if err := attest(c, "claude-code", opSSHRegister, json.RawMessage(sshArgs)); err == nil {
			t.Error("claude-code used the T3 plugin's grant")
		}
	})
	decide(t, c, t3Plugin, "claude-code.ssh_target", payload, func(string) {
		if err := attest(c, t3Plugin, t3SSHOp, json.RawMessage(sshArgs)); err == nil {
			t.Error("a grant of another plugin's kind registered a target")
		}
	})
	decide(t, c, t3Plugin, "t3agents.role_widen", payload, func(string) {
		if err := attest(c, t3Plugin, t3SSHOp, json.RawMessage(sshArgs)); err == nil {
			t.Error("an approval of another kind allowed a registration")
		}
	})
	if err := attest(c, t3Plugin, t3SSHOp, json.RawMessage(sshArgs)); err == nil {
		t.Fatal("the grant outlived the approval")
	}
	if _, err := c.attestHomeCall(t3Plugin, t3SSHOp, json.RawMessage(`{"name":"t","host":"a.example.com","Host":"b.example.com"}`)); err == nil || !strings.Contains(err.Error(), "bad ssh.register") {
		t.Errorf("folded keys: %v", err)
	}
}

// The relay itself runs the gate for the T3 op names, before it looks for a device.
func TestHomeCallRunsTheGateForT3Ops(t *testing.T) {
	c := targetCore(t)
	for _, op := range []string{t3SSHOp, t3PutOp} {
		args := json.RawMessage(sshArgs)
		if op == t3PutOp {
			args = putArgs("apr_made_up", "")
		}
		_, err := c.homeCall(context.Background(), t3Plugin, []string{"t3-agent"}, "", op, args)
		if err == nil || !strings.Contains(err.Error(), "Rubi refused") {
			t.Errorf("%s: %v", op, err)
		}
	}
	// the manifest's capability decides which ops a plugin may send
	if _, err := c.homeCall(context.Background(), t3Plugin, []string{"t3-setup"}, "", t3SSHOp, json.RawMessage(sshArgs)); err == nil || !strings.Contains(err.Error(), "doesn't allow") {
		t.Errorf("t3-agent op with only t3-setup: %v", err)
	}
	if _, err := c.homeCall(context.Background(), "claude-code", []string{"claude-agent"}, "", t3SSHOp, json.RawMessage(sshArgs)); err == nil || !strings.Contains(err.Error(), "doesn't allow") {
		t.Errorf("claude-agent plugin sending a t3-agent op: %v", err)
	}
}

func TestT3CapabilitiesAreOffered(t *testing.T) {
	for _, h := range []string{"t3-agent", "t3-setup"} {
		if homeCapabilities[h] == "" {
			t.Errorf("no install-screen text for %s", h)
		}
	}
	if !strings.Contains(homeCapabilities["t3-setup"], "administrator") || !strings.Contains(homeCapabilities["t3-setup"], "checked against fixed hashes") {
		t.Errorf("t3-setup text: %s", homeCapabilities["t3-setup"])
	}
}

// The skill library is read by the plugins that have roles: the claude-agent one and the T3 one, not others.
func TestLibraryGetIsForRolePlugins(t *testing.T) {
	c := libCore(t)
	for home, wantDeclared := range map[string]bool{"claude-agent": true, "t3-agent": true, "t3-setup": false, "agent-setup": false, "hue": false} {
		_, err := c.libraryHostGet(context.Background(), []string{home}, json.RawMessage(`{"id":"nothing","version":1}`))
		declared := err == nil || !strings.Contains(err.Error(), "doesn't declare")
		if declared != wantDeclared {
			t.Errorf("%s: declared=%v err=%v", home, declared, err)
		}
	}
}
