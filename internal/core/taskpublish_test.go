package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/agenttask"
	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

type taskBrokerFixture struct {
	s         agenttask.State
	token     string
	publishes int
	fail      bool
}

func (f *taskBrokerFixture) Call(ctx context.Context, op string, args, out any) error {
	switch op {
	case "t3-agent.task.inspect":
		*(out.(*agenttask.State)) = f.s
		return nil
	case "t3-agent.task.publish":
		b, _ := json.Marshal(args)
		var in struct{ ID, Token string }
		_ = json.Unmarshal(b, &in)
		f.token = in.Token
		f.publishes++
		if f.fail {
			return errors.New("fake-provider-detail-never-forward")
		}
		*(out.(*json.RawMessage)) = json.RawMessage(`{"phase":"finished"}`)
		return nil
	default:
		return errors.New("unexpected operation")
	}
}
func brokerFixture(t *testing.T) (*Core, *taskBrokerFixture, json.RawMessage) {
	t.Helper()
	c := targetCore(t)
	id := "task_0123456789abcdef01234567"
	spec := agenttask.Spec{Project: "a", Role: "role", From: "phone", Bot: "bot", MessageID: "message", Target: &agenttask.Target{Repository: "owner/repo", Base: "main", Account: "github-fixture"}}
	proof, err := c.taskProof("device-a", spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.Proof = proof
	f := &taskBrokerFixture{s: agenttask.State{Version: 1, ID: id, Workspace: agenttask.Workspace{ID: id, Project: "a", Role: "role", Branch: "rubi/" + id}, Spec: spec, Phase: agenttask.Waiting}}
	if err := c.Vault.Update(func(d *vault.Data) error {
		d.Integrations[plugins.OfficialT3PluginID] = &vault.Integration{Enabled: true, Accounts: []*vault.Account{{ID: "github-fixture", Secrets: map[string]string{"github_token": "old-fake-credential"}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"id": id})
	return c, f, b
}
func TestTaskBrokerResolvesRotatedCredentialAtUseTimeAndReturnsNoSecret(t *testing.T) {
	c, f, args := brokerFixture(t)
	if err := c.Vault.Update(func(d *vault.Data) error {
		d.Integrations[plugins.OfficialT3PluginID].Accounts[0].Secrets["github_token"] = "rotated-fake-credential"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out, err := c.publishTask(context.Background(), plugins.OfficialT3PluginID, "device-a", args, f)
	if err != nil || f.token != "rotated-fake-credential" {
		t.Fatal("credential not resolved now", err)
	}
	if strings.Contains(string(out), "credential") || strings.Contains(string(out), f.s.Spec.Proof) {
		t.Fatal("private credential/mandate returned")
	}
	f.fail = true
	_, err = c.publishTask(context.Background(), plugins.OfficialT3PluginID, "device-a", args, f)
	if err == nil || strings.Contains(err.Error(), "fake-provider-detail") {
		t.Fatal("Home detail forwarded")
	}
}
func TestTaskBrokerRefusesTamperingRevocationAndCredentialOverrides(t *testing.T) {
	for _, scenario := range []string{"device", "repository", "base", "account", "project", "requester", "request-id", "proof", "revoked", "disabled", "account-label", "caller-token", "caller-target", "foreign-plugin"} {
		t.Run(scenario, func(t *testing.T) {
			c, f, args := brokerFixture(t)
			device := "device-a"
			plugin := plugins.OfficialT3PluginID
			switch scenario {
			case "device":
				device = "device-b"
			case "repository":
				f.s.Spec.Target.Repository = "other/repo"
			case "base":
				f.s.Spec.Target.Base = "other"
			case "account":
				f.s.Spec.Target.Account = "other"
			case "project":
				f.s.Spec.Project = "other"
				f.s.Workspace.Project = "other"
			case "requester":
				f.s.Spec.From = "other"
			case "request-id":
				f.s.Spec.MessageID = "other"
			case "proof":
				f.s.Spec.Proof = "invented"
			case "account-label":
				_ = c.Vault.Update(func(d *vault.Data) error {
					a := d.Integrations[plugins.OfficialT3PluginID].Accounts[0]
					a.ID = "different-id"
					a.Label = "github-fixture"
					return nil
				})
			case "revoked":
				_ = c.Vault.Update(func(d *vault.Data) error {
					delete(d.Integrations[plugins.OfficialT3PluginID].Accounts[0].Secrets, "github_token")
					return nil
				})
			case "disabled":
				_ = c.Vault.Update(func(d *vault.Data) error { d.Integrations[plugins.OfficialT3PluginID].Enabled = false; return nil })
			case "caller-token":
				args = json.RawMessage(`{"id":"` + f.s.ID + `","token":"injected"}`)
			case "caller-target":
				args = json.RawMessage(`{"id":"` + f.s.ID + `","repository":"other/repo"}`)
			case "foreign-plugin":
				plugin = "untrusted"
			}
			if _, err := c.publishTask(context.Background(), plugin, device, args, f); err == nil || f.publishes != 0 {
				t.Fatal("unapproved publication reached Home")
			}
		})
	}
}
func TestTaskBrokerPrivateInspectCannotBeCalledByPlugin(t *testing.T) {
	c := targetCore(t)
	for _, op := range []string{"t3-agent.task.inspect", "claude-agent.task.inspect", "claude-agent.task.publish"} {
		if _, err := c.homeCall(context.Background(), plugins.OfficialT3PluginID, []string{"t3-agent", "claude-agent"}, "", op, json.RawMessage(`{}`)); err == nil {
			t.Fatal("private broker operation escaped")
		}
	}
}
func TestTaskStartProofRequiresExactConfirmation(t *testing.T) {
	c, _, _ := brokerFixture(t)
	args := json.RawMessage(`{"project":"a","role":"role","from":"phone","message_id":"message","task":true,"publish":{"repository":"owner/repo","base":"main","account":"github-fixture"}}`)
	payload, _ := json.Marshal(map[string]any{"device": "device-a", "args": args})
	end := c.beginExec(plugins.OfficialT3PluginID, "t3agents.task_start", payload)
	defer end()
	out, err := c.attestHomeCallForDevice(plugins.OfficialT3PluginID, "device-a", "t3-agent.start", args)
	if err != nil {
		t.Fatal(err)
	}
	var spec agenttask.Spec
	_ = json.Unmarshal(out, &spec)
	expected, err := c.taskProof("device-a", spec)
	if err != nil || spec.Proof != expected {
		t.Fatal("confirmed target not signed")
	}
	if _, err := c.attestHomeCallForDevice(plugins.OfficialT3PluginID, "device-a", "t3-agent.start", out); err == nil {
		t.Fatal("approval/proof replay permitted")
	}
}
