package core

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Deikus-LXXVII/rubi/internal/agenttask"
	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// Only the device-bound mandate is signed, never a credential. The vault account
// is looked up anew after verification, so revocation/rotation takes effect now.
func (c *Core) taskProof(device string, s agenttask.Spec) (string, error) {
	if c.Vault == nil || device == "" || s.Project == "" || s.Role == "" || s.From == "" || s.MessageID == "" || s.Target == nil || s.Target.Validate() != nil {
		return "", errors.New("task: explicit project, role, requester, message ID and publish target required")
	}
	key, err := c.Vault.Key()
	if err != nil {
		return "", errors.New("task: vault unavailable")
	}
	b, _ := json.Marshal(struct {
		Device, Project, Role, From, Bot, MessageID string
		Target                                      agenttask.Target
	}{device, s.Project, s.Role, s.From, s.Bot, s.MessageID, *s.Target})
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte("rubi/t3-task-publish/v1\x00"))
	_, _ = h.Write(b)
	return "v1." + hex.EncodeToString(h.Sum(nil)), nil
}
func (c *Core) signTaskArgs(device string, args json.RawMessage) (json.RawMessage, error) {
	var s agenttask.Spec
	var raw map[string]json.RawMessage
	if json.Unmarshal(args, &s) != nil || json.Unmarshal(args, &raw) != nil {
		return nil, errors.New("task: invalid target arguments")
	}
	for k := range raw {
		if strings.EqualFold(k, "publish_proof") || strings.EqualFold(k, "token") {
			return nil, errors.New("task: caller cannot supply publication proof or credential")
		}
	}
	if s.Target == nil {
		return args, nil
	}
	proof, err := c.taskProof(device, s)
	if err != nil {
		return nil, err
	}
	raw["publish_proof"], _ = json.Marshal(proof)
	return json.Marshal(raw)
}

type taskHomeCaller interface {
	Call(context.Context, string, any, any) error
}

func (c *Core) publishTask(ctx context.Context, plugin, device string, args json.RawMessage, cl taskHomeCaller) (json.RawMessage, error) {
	denied := errors.New("task: publication mandate or vault credential unavailable")
	if plugin != plugins.OfficialT3PluginID || noFoldedDuplicates(args) != nil {
		return nil, denied
	}
	var in struct {
		ID string `json:"id"`
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(args, &in) != nil || json.Unmarshal(args, &raw) != nil || len(raw) != 1 || !agenttask.ValidID(in.ID) {
		return nil, denied
	}
	var s agenttask.State
	if err := cl.Call(ctx, "t3-agent.task.inspect", in, &s); err != nil {
		return nil, errors.New("task: trusted status unavailable")
	}
	if s.ID != in.ID || s.Validate() != nil {
		return nil, denied
	}
	if s.Phase.Terminal() {
		return json.Marshal(s.Public())
	}
	token := ""
	if s.Spec.Target != nil {
		proof, err := c.taskProof(device, s.Spec)
		if err != nil || !hmac.Equal([]byte(proof), []byte(s.Spec.Proof)) {
			return nil, denied
		}
		err = c.Vault.View(func(d *vault.Data) error {
			if i := d.Integrations[plugins.OfficialT3PluginID]; i == nil || !i.Enabled {
				return denied
			}
			a, e := accountOf(d, plugins.OfficialT3PluginID, s.Spec.Target.Account)
			if e != nil || a.ID != s.Spec.Target.Account {
				return denied
			}
			token = a.Secrets["github_token"]
			return nil
		})
		if err != nil || token == "" || len(token) > 4096 || strings.ContainsAny(token, "\r\n\x00") {
			return nil, denied
		}
	}
	var out json.RawMessage
	if err := cl.Call(ctx, "t3-agent.task.publish", struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}{in.ID, token}, &out); err != nil {
		return nil, errors.New("task: publication pending; retry the same task after checking status")
	}
	return out, nil
}

// Ticket links are minted by Core, never by Home or agent-produced text. They
// open only the official plugin setup page; a setup ticket cannot drive settings.
func (c *Core) taskPanelReply(op string, out json.RawMessage) json.RawMessage {
	if op != "t3-agent.task.status" && op != "t3-agent.task.publish" {
		return out
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(out, &obj) != nil || obj == nil {
		return out
	}
	if link, err := c.Link("setup:" + plugins.OfficialT3PluginID); err == nil {
		obj["panel_url"], _ = json.Marshal(link)
		if b, e := json.Marshal(obj); e == nil {
			return b
		}
	}
	return out
}
