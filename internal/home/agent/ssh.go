package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/sshtarget"
)

// SSH targets (design agent-roles section 9, contract in docs/design/agent-runner.md "ssh.*"). Home is the SSH
// client: the registry, the keys and the host-key pinning are in package sshtarget; this file is the seam to the
// Manager (the ssh.* operations of the plugin) and to the runner channel (the runner's ssh.* messages).

// SSHOpenParams is a runner's ssh.open.
type SSHOpenParams struct {
	Target    string `json:"target"`
	Command   string `json:"command"`
	SessionID string `json:"session_id"`
	ToolUseID string `json:"tool_use_id,omitempty"`
}

var sshIDRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// SSHOpened is the answer to ssh.open. The output starts to flow after this answer is sent.
type SSHOpened struct {
	Stream string `json:"stream"`
	s      *sshtarget.Stream
}

func (o *SSHOpened) afterReply() { o.s.Start() }

func (m *Manager) initSSH() {
	if m.opt.Dir == "" {
		return
	}
	st, err := sshtarget.Open(filepath.Join(m.opt.Dir, sshtarget.DirName), m.opt.SSHDial, m.opt.Now)
	if err != nil {
		log.Printf("ssh targets aren't available: %v", err)
		return
	}
	m.ssh = sshtarget.NewHub(sshtarget.Config{Store: st, Audit: sshtarget.NewAudit(st.Dir()), Limits: m.opt.SSHLimits,
		Granted: m.sshGranted, Redact: func(s string) string { return string(RedactCredentials([]byte(s))) }, Logf: log.Printf})
}

// sshGranted says whether the role (a role id) has the target in its record now. The legacy role has no grants.
func (m *Manager) sshGranted(role, target string) bool {
	if m.roles == nil || !roles.IsID(role) {
		return false
	}
	r, err := m.roles.Get(role, 0)
	return err == nil && r.Status != "archived" && slices.Contains(r.SSH, target)
}

// sshRecheck cuts the live streams whose grant is gone (after a role changed).
func (m *Manager) sshRecheck() {
	if m.ssh != nil {
		m.ssh.Recheck()
	}
}

func decodeStrict(args json.RawMessage, v any) error {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	d := json.NewDecoder(bytes.NewReader(args))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("bad arguments: %s", clip(err.Error(), 120))
	}
	return nil
}

// sshOp serves ssh.scan, ssh.register, ssh.list, ssh.remove and ssh.audit (op without the "claude-agent." prefix).
// The plugin gates them; Home does not decide who may call them.
func (m *Manager) sshOp(ctx context.Context, op string, args json.RawMessage) (any, error) {
	if m.ssh == nil {
		return nil, errors.New("SSH targets aren't available here")
	}
	st := m.ssh.Store()
	switch op {
	case "ssh.scan":
		var in struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		}
		if err := decodeStrict(args, &in); err != nil {
			return nil, err
		}
		return st.Scan(ctx, in.Host, in.Port)
	case "ssh.register":
		var in sshtarget.RegisterArgs
		if err := decodeStrict(args, &in); err != nil {
			return nil, err
		}
		line, err := st.Register(ctx, in)
		if err != nil {
			return nil, err
		}
		return map[string]any{"public_key_line": line}, nil
	case "ssh.list":
		if err := decodeStrict(args, &struct{}{}); err != nil {
			return nil, err
		}
		type row struct {
			sshtarget.View
			Roles []map[string]string `json:"roles"`
		}
		rows := []row{}
		granted := map[string][]map[string]string{}
		if m.roles != nil {
			for _, r := range m.roles.List("") {
				if r.Legacy {
					continue
				}
				if full, err := m.roles.Get(r.ID, 0); err == nil {
					for _, t := range full.SSH {
						granted[t] = append(granted[t], map[string]string{"id": r.ID, "name": r.Name, "project": r.Project})
					}
				}
			}
		}
		for _, v := range st.List() {
			g := granted[v.Name]
			if g == nil {
				g = []map[string]string{}
			}
			rows = append(rows, row{View: v, Roles: g})
		}
		return map[string]any{"targets": rows}, nil
	case "ssh.remove":
		var in struct {
			Name string `json:"name"`
		}
		if err := decodeStrict(args, &in); err != nil {
			return nil, err
		}
		if err := st.Remove(in.Name); err != nil {
			return nil, err
		}
		n := m.ssh.CloseWhere(func(_, target string) bool { return target == in.Name }, "revoked")
		return map[string]any{"ok": true, "closed_streams": n}, nil
	case "ssh.audit":
		var in struct {
			Target   string  `json:"target"`
			Limit    int     `json:"limit"`
			AfterSeq *uint64 `json:"after_seq"`
		}
		if err := decodeStrict(args, &in); err != nil {
			return nil, err
		}
		if in.AfterSeq != nil { // paging forward: oldest first, from the entry after after_seq
			es, last := m.ssh.Audit().After(in.Target, *in.AfterSeq, in.Limit)
			return map[string]any{"entries": es, "last_seq": last}, nil
		}
		_, last := m.ssh.Audit().After("", ^uint64(0), 1)
		return map[string]any{"entries": m.ssh.Audit().Recent(in.Target, in.Limit), "last_seq": last}, nil
	}
	return nil, fmt.Errorf("Rubi Home doesn't do %q", op)
}

// errSSHNotAvailable is all a role learns when it can't use a target: ungranted and unknown look the same.
const errSSHNotAvailable = "this SSH target isn't available to this role"

// SSHOpen serves ssh.open of the runner of h.p. Everything is checked against Home's own state: the role is the
// VM's, the grant is read from the role record, the target and its rules from the registry.
func (h *handler) SSHOpen(ctx context.Context, p SSHOpenParams, notify func(string, any)) (*SSHOpened, error) {
	m := h.m
	if m.ssh == nil {
		return nil, errors.New("SSH targets aren't available here")
	}
	if !sshIDRE.MatchString(p.ToolUseID) { // goes into items and the audit: a short plain token or nothing
		p.ToolUseID = ""
	}
	req := sshtarget.OpenReq{Role: h.p.key(), Project: h.p.ID, Target: clip(p.Target, 64), Command: p.Command}
	m.mu.Lock()
	if sl := m.ownerLocked(h.p, p.SessionID); sl != nil {
		req.Bot = sl.bot
	} else {
		req.Bot = m.sessionBot[h.p.key()+"|"+p.SessionID]
	}
	m.mu.Unlock()
	refuse := func(reason, msg string) (*SSHOpened, error) {
		m.ssh.Refused(req, "refused", reason)
		return nil, &RPCError{Code: -32000, Message: msg}
	}
	if !m.ssh.AllowOpen(req.Role) {
		return refuse("too many requests", "too many SSH requests; wait a minute")
	}
	if p.SessionID != "" && !okID(p.SessionID) {
		req.Bot = ""
		return refuse("bad session id", "bad session id")
	}
	if p.Command == "" || len(p.Command) > sshtarget.MaxCommandLen {
		return refuse("bad command", "an SSH command is needed (no interactive shell)")
	}
	if h.p.Role == "" {
		return refuse("the default role has no SSH grants", errSSHNotAvailable)
	}
	if !sshtarget.ValidName(p.Target) || !m.sshGranted(req.Role, p.Target) {
		return refuse("not granted to the role", errSSHNotAvailable)
	}
	tg, ok := m.ssh.Store().Get(p.Target)
	if !ok {
		return refuse("unknown target", errSSHNotAvailable)
	}
	if !tg.Allows(p.Command) {
		return refuse("command not allowed", "this command isn't allowed on the target")
	}
	if tg.AskEachTime {
		// The user approves exactly what will run: the command is shown as it is (redaction is for the audit copy
		// only). A command with characters that can't be shown faithfully is refused instead of being asked about.
		if !sshtarget.Displayable(p.Command) {
			return refuse("command has control or format characters", "this command has characters that can't be shown for approval")
		}
		end, ok := m.ssh.BeginAsk(req.Role)
		if !ok {
			return refuse("too many questions waiting", "this role has SSH questions waiting for the user already; wait for them")
		}
		defer end()
		in, _ := json.Marshal(map[string]any{"target": tg.Name, "host": tg.Host, "port": tg.Port, "user": tg.User, "command": p.Command})
		it := Item{Type: "ssh.ask", Project: h.p.ID, Role: h.p.key(), SessionID: p.SessionID, ToolUseID: p.ToolUseID,
			Host: tg.Host, Op: "ssh", Input: in}
		if r := m.waitDecision(ctx, h.p.ID, it); r.Behavior != "allow" {
			msg := r.Message
			if msg == "" {
				msg = "Rubi: the SSH command wasn't approved."
			}
			m.ssh.Refused(req, "denied", clip(msg, 200))
			return nil, &RPCError{Code: -32000, Message: msg}
		}
		if !m.sshGranted(req.Role, p.Target) { // revoked while the question was open
			return refuse("revoked while waiting for approval", errSSHNotAvailable)
		}
	}
	s, err := m.ssh.Open(ctx, req, notify)
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: err.Error()}
	}
	return &SSHOpened{Stream: s.ID(), s: s}, nil
}

// SSHNote serves the runner's ssh.data, ssh.ack and ssh.close notifications. A stream answers only to the
// runner of the role that opened it.
func (h *handler) SSHNote(method string, params json.RawMessage) {
	if h.m.ssh == nil {
		return
	}
	var n struct {
		Stream string `json:"stream"`
		FD     int    `json:"fd"`
		B64    string `json:"b64"`
		EOF    bool   `json:"eof"`
		N      int64  `json:"n"`
	}
	if json.Unmarshal(params, &n) != nil || n.Stream == "" {
		return
	}
	owner := h.p.key()
	switch method {
	case "ssh.data":
		b, err := base64.StdEncoding.DecodeString(n.B64)
		if err != nil {
			h.m.ssh.Close(owner, n.Stream)
			return
		}
		h.m.ssh.Data(owner, n.Stream, n.FD, b, n.EOF)
	case "ssh.ack":
		h.m.ssh.Ack(owner, n.Stream, n.N)
	case "ssh.close":
		h.m.ssh.Close(owner, n.Stream)
	}
}
