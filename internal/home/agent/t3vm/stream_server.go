package t3vm

// The session server's side of a streamed session (stream.go): a T3 session in a VM Home runs itself (the Linux
// microVM, agent.StreamSessionHost). The checks are the container path's: the role, the clamped arguments, the caps,
// the same SessionSpec resolution of every folder; the VM gets exactly those folders, at the host's paths, and the
// policy files in folders of their own; the network pipe serves the same proxy, hook gate and T3 forward. Claude then
// runs in the VM by a guest exec, its stdio relayed over the shim's connection.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/t3"
)

// claudeForArch is claude's place in the runner image built for goarch (npm installs the Agent SDK's build for the
// image's own CPU).
func claudeForArch(goarch string) string {
	cpu := "arm64"
	if goarch == "amd64" {
		cpu = "x64"
	}
	return "/opt/rubi-runner/node_modules/@anthropic-ai/claude-agent-sdk-linux-" + cpu + "/claude"
}

// policyDirs holds the managed settings and the hook script in two folders of their own (Dir/t3vm/vm-etc/claude-code
// and .../rubi), shared read-only at the folders of ManagedSettingsVMPath and HookVMPath: a VM Home runs shares
// folders, not single files, and the folder they are written to first (Dir/t3vm) also holds the proxy's passwords.
func (s *Server) policyDirs(files sessionFiles) ([]agent.Mount, error) {
	root := filepath.Join(s.opt.Dir, "t3vm", "vm-etc")
	var out []agent.Mount
	for _, f := range []struct{ src, vmPath string }{{files.settings, ManagedSettingsVMPath}, {files.hook, HookVMPath}} {
		body, err := os.ReadFile(f.src)
		if err != nil {
			return nil, err
		}
		dir := filepath.Join(root, filepath.Base(filepath.Dir(f.vmPath)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if err := putFile(filepath.Join(dir, filepath.Base(f.vmPath)), body, 0o644); err != nil {
			return nil, err
		}
		out = append(out, agent.Mount{Source: dir, Target: filepath.Dir(f.vmPath), ReadOnly: true})
	}
	return out, nil
}

// Mounts are the folders of a streamed session's VM: the resolved binds of the spec (the workspace, the role's
// directories, the sign-in, the read-only extras), parents first, then the policy folders.
func (s SessionSpec) Mounts(policy []agent.Mount) ([]agent.Mount, error) {
	if s.Role == "" {
		return nil, fmt.Errorf("t3vm: no role")
	}
	if s.T3Port != 0 && (s.T3Port < 1024 || s.T3Port > 65535 || (s.T3Port >= 3128 && s.T3Port <= 3130)) {
		return nil, fmt.Errorf("t3vm: bad T3 port")
	}
	r, err := s.resolve()
	if err != nil {
		return nil, err
	}
	out := make([]agent.Mount, 0, len(r.binds)+len(policy))
	for _, b := range r.binds {
		out = append(out, agent.Mount{Source: b.src, Target: b.dst, ReadOnly: b.readOnly})
	}
	for _, p := range policy {
		if !p.ReadOnly {
			return nil, fmt.Errorf("t3vm: a policy folder must be read-only")
		}
		for _, b := range r.binds {
			if !b.readOnly && within(b.src, p.Source) {
				return nil, fmt.Errorf("t3vm: a read-write folder contains the policy folder %s", p.Target)
			}
		}
	}
	return append(out, policy...), nil
}

// streamSession is what a streamed session runs once Home has answered: claude in its VM.
type streamSession struct {
	vm   agent.SessionVM
	dir  string
	env  map[string]string
	argv []string
}

// startStream is start for a backend with a StreamSessionHost. ses.stream holds what runs after the answer.
func (s *Server) startStream(ctx context.Context, end func(), req StartRequest) (_ *session, err error) {
	o := s.opt
	role, err := o.Backend.T3Role(req.Role)
	if err != nil {
		return nil, err
	}
	ses := &session{s: s, role: role.ID, giveUp: end}
	defer func() {
		if err != nil {
			ses.teardown()
		}
	}()
	ses.onEnd(role.Release)
	policy := Policy{Ceiling: t3.Mode(role.MaxMode), Cwd: req.Cwd, Attachments: o.Attachments, T3Port: o.T3Port}
	argv, err := ClampArgv(req.Argv, policy)
	if err != nil {
		return nil, err
	}
	release, err := s.admitWait(ctx, role.ID)
	if err != nil {
		return nil, err
	}
	ses.slot = release
	files, err := s.writeFiles()
	if err != nil {
		return nil, err
	}
	policyMounts, err := s.policyDirs(files)
	if err != nil {
		return nil, err
	}
	// The session's own folders are checked as the container path checks them (the workspace inside the role's project,
	// apart from Home's files)...
	spec := SessionSpec{
		Name: "rubi-t3-check", Role: role.ID,
		ClaudeDir: role.ClaudeDir, ContentDir: role.ContentDir, Cwd: req.Cwd, TrustedRoot: role.ProjectDir,
		HelperDir: o.Dir, SignInDir: files.signIn, ReadOnly: ExtraDirs(argv, req.Cwd),
		HookScript: files.hook, ManagedSettings: files.settings, T3Port: o.T3Port,
	}
	if _, err := spec.Mounts(policyMounts); err != nil {
		return nil, err
	}
	sid := argv.SessionID() // the gate names a session's thread by it (T3 always passes one; a probe has none)
	// ...and the role's VM has the project's root and T3's attachments folder: every folder a session of the role may
	// work in or add (ClampArgv allows only the thread's folder and the attachments).
	vmSpec := spec
	vmSpec.Cwd, vmSpec.ReadOnly = role.ProjectDir, nil
	if o.Attachments != "" {
		if err := os.MkdirAll(o.Attachments, 0o700); err != nil {
			return nil, err
		}
		vmSpec.ReadOnly = []string{o.Attachments}
	}
	v, releaseVM, err := s.pool.acquire(ctx, role.ID, func(v *roleVM) error {
		name, err := NewName(role.ID)
		if err != nil {
			return err
		}
		vmSpec.Name = name
		mounts, err := vmSpec.Mounts(policyMounts)
		if err != nil {
			return err
		}
		return s.startRoleVM(ctx, v, role, vmSpec, mounts)
	}, o.Logf)
	if err != nil {
		return nil, err
	}
	ses.onEnd(releaseVM)
	if sid != "" {
		v.mu.Lock()
		if _, taken := v.threads[sid]; taken {
			v.mu.Unlock()
			return nil, fmt.Errorf("t3vm: claude session %s already runs in the role's VM", clipStr(sid, 64))
		}
		v.threads[sid] = threadOf(req.Env)
		v.mu.Unlock()
		ses.onEnd(func() { v.mu.Lock(); delete(v.threads, sid); v.mu.Unlock() })
	}
	// the VM stopping (its pipe failing, Home stopping) ends the session
	stopAfter := context.AfterFunc(v.ctx, end)
	ses.onEnd(func() { stopAfter() })

	env, err := spec.EnvMap(req.Env, v.proxyEnv)
	if err != nil {
		return nil, err
	}
	for k, val := range o.testEnv {
		env[k] = val
	}
	claude := o.StreamClaude
	if claude == "" {
		claude = claudeForArch(runtime.GOARCH)
	}
	vm := v.vm
	ses.stream = &streamSession{vm: vm, dir: req.Cwd, env: env, argv: append([]string{claude}, argv.args...)}
	return ses, nil
}

// serveStream runs the session's process and relays it over the shim's connection until either ends.
func (s *Server) serveStream(ctx context.Context, ses *session, in io.Reader, w io.Writer) {
	st := ses.stream
	out := &streamOut{w: w}
	ectx, cancel := context.WithTimeout(ctx, time.Minute)
	p, err := st.vm.ExecIn(ectx, st.dir, st.env, st.argv, out.stderrWriter())
	cancel()
	if err != nil {
		s.opt.Logf("t3vm: running claude in the session's VM: %v", err)
		_ = out.write(chunkStderr, []byte("rubi-home agent-vm: claude didn't start in the VM: "+clipStr(err.Error(), 300)+"\n"))
		_ = out.write(chunkExit, []byte{0, 0, 0, 1})
		return
	}
	if err := streamServer(ctx, in, out, p); err != nil {
		s.opt.Logf("t3vm: session of role %s: %v", ses.role, err)
	}
}

// probeVersionStream asks a throwaway session VM, with no folders and no pipe, for claude's version.
func (s *Server) probeVersionStream(ctx context.Context) (string, error) {
	if err := s.opt.Stream.Ready(); err != nil {
		return "", fmt.Errorf("the VM: %w", err)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	vm, err := s.opt.Stream.StartSessionVM(ctx, ContainerPrefix+"ver-"+hex.EncodeToString(b), nil)
	if err != nil {
		return "", fmt.Errorf("starting a VM for claude's version: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), stopLimit)
		defer cancel()
		_ = vm.Stop(sctx)
	}()
	claude := s.opt.StreamClaude
	if claude == "" {
		claude = claudeForArch(runtime.GOARCH)
	}
	p, err := vm.ExecIn(ctx, "/", map[string]string{}, []string{claude, "--version"}, nil)
	if err != nil {
		return "", fmt.Errorf("asking the image for claude's version: %w", err)
	}
	defer p.Close()
	stop := context.AfterFunc(ctx, func() { _ = p.Close() }) // a hung claude doesn't hold the probe past its time
	defer stop()
	out, err := io.ReadAll(io.LimitReader(p, 4096))
	if err != nil {
		return "", err
	}
	if code, err := p.Wait(); err != nil || code != 0 {
		return "", fmt.Errorf("claude --version ended with %d", code)
	}
	v := strings.TrimSpace(string(out))
	if v == "" || len(v) > 200 || strings.Contains(v, "\n") {
		return "", errors.New("claude's version was not readable")
	}
	return v, nil
}
