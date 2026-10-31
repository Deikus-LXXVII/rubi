package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// AppleContainer runs a project in a VM of Apple's container tool (github.com/apple/container, 1.6.x),
// by shelling out to its CLI. Every container gets the image below, the project at /work, a non-root
// user, no capabilities, and a host-only network: the only way out is the allow-list proxy on the Mac.
type AppleContainer struct {
	// Bin is the container CLI ("container" when empty, looked up in PATH).
	Bin string
	// Image is the runner image's tag; RunnerDir the directory with runner/ (its container/Dockerfile) to
	// build it from when the image is missing; "" means it must exist already.
	Image     string
	RunnerDir string
	// Network is the host-only network the container joins (created on first use).
	Network string
	// CPUs and Memory limit the VM ("" = the CLI's defaults).
	CPUs   string
	Memory string
	// EnvDir is where the exec's environment file is written (0600, in a 0700 directory of its own).
	EnvDir string

	// run starts a CLI command to completion and returns its combined output; spawn starts one with
	// pipes. Both are seams for tests; nil uses os/exec.
	run   func(ctx context.Context, argv []string) ([]byte, error)
	spawn func(ctx context.Context, argv []string) (Conn, error)

	mu    sync.Mutex
	name  string
	proj  Project
	state State
}

const (
	// DefaultImage is the runner image's tag.
	DefaultImage   = "rubi-agent-runner:1"
	DefaultNetwork = "rubi-agent"
	// runnerUID is the non-root user inside the image (see runner/container/Dockerfile).
	runnerUID = "10001"
	// RunnerCmd is how the image starts the runner (the exec in the VM).
	runnerMain = "/opt/rubi-runner/dist/main.js"
)

func (a *AppleContainer) bin() string {
	if a.Bin != "" {
		return a.Bin
	}
	return "container"
}

func (a *AppleContainer) image() string {
	if a.Image != "" {
		return a.Image
	}
	return DefaultImage
}

func (a *AppleContainer) network() string {
	if a.Network != "" {
		return a.Network
	}
	return DefaultNetwork
}

var nameUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// containerName is the container's id for a project: stable, so a leftover from a crash is found and
// removed by the next start.
func containerName(p Project) string {
	sum := sha256.Sum256([]byte(p.Dir))
	slug := strings.Trim(nameUnsafe.ReplaceAllString(strings.ToLower(p.ID), "-"), "-")
	if len(slug) > 24 {
		slug = slug[:24]
	}
	return "rubi-agent-" + slug + "-" + hex.EncodeToString(sum[:4])
}

// BuildArgs is the image build: the context is the runner directory.
func (a *AppleContainer) BuildArgs() []string {
	return []string{a.bin(), "build", "--progress", "plain", "-t", a.image(),
		"-f", filepath.Join(a.RunnerDir, "container", "Dockerfile"), a.RunnerDir}
}

func (a *AppleContainer) imageInspectArgs() []string {
	return []string{a.bin(), "image", "inspect", a.image()}
}

func (a *AppleContainer) networkCreateArgs() []string {
	return []string{a.bin(), "network", "create", "--internal", a.network()}
}

// RunArgs starts the VM detached: the project at /work, the host-only network, no capabilities, a
// read-only root with a tmpfs for /tmp, a non-root user. The container just waits; the runner is started by
// exec so its environment (the run token) never appears on a command line.
func (a *AppleContainer) RunArgs(p Project) []string {
	args := []string{a.bin(), "run", "-d", "--name", containerName(p), "--init",
		"--network", a.network(), "--user", runnerUID, "--cap-drop", "ALL", "--read-only",
		"--tmpfs", "/tmp",
		"--mount", "type=bind,source=" + p.Dir + ",target=/work",
		"--workdir", "/work"}
	if a.CPUs != "" {
		args = append(args, "--cpus", a.CPUs)
	}
	if a.Memory != "" {
		args = append(args, "--memory", a.Memory)
	}
	return append(args, "--entrypoint", "sleep", a.image(), "infinity")
}

// ExecArgs runs argv in the running container with stdin open; env goes through an environment file.
func (a *AppleContainer) ExecArgs(p Project, envFile string, argv []string) []string {
	args := []string{a.bin(), "exec", "-i", "--user", runnerUID, "--workdir", "/work"}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	args = append(args, containerName(p))
	return append(args, argv...)
}

func (a *AppleContainer) stopArgs(p Project) []string {
	return []string{a.bin(), "stop", "--time", "5", containerName(p)}
}

func (a *AppleContainer) deleteArgs(p Project) []string {
	return []string{a.bin(), "delete", "--force", containerName(p)}
}

// EnvFile renders env as the file --env-file reads (KEY=value lines, sorted). A value with a newline is
// refused: it would be a second variable.
func EnvFile(env map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	for _, k := range keys {
		if k == "" || strings.ContainsAny(k, "=\n\r#") || strings.ContainsAny(env[k], "\n\r") {
			return nil, fmt.Errorf("environment variable %q can't be passed", k)
		}
		b.WriteString(k + "=" + env[k] + "\n")
	}
	return b.Bytes(), nil
}

func (a *AppleContainer) setState(s State) {
	a.mu.Lock()
	a.state = s
	a.mu.Unlock()
}

// State implements VM.
func (a *AppleContainer) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == "" {
		return StateStopped
	}
	return a.state
}

func (a *AppleContainer) do(ctx context.Context, argv []string) ([]byte, error) {
	if a.run != nil {
		return a.run(ctx, argv)
	}
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", strings.Join(argv[:min(len(argv), 3)], " "), err, tail(out, 400))
	}
	return out, nil
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.TrimSpace(string(b))
}

// EnsureImage builds the runner image when it isn't there yet.
func (a *AppleContainer) EnsureImage(ctx context.Context) error {
	if _, err := a.do(ctx, a.imageInspectArgs()); err == nil {
		return nil
	}
	if a.RunnerDir == "" {
		return fmt.Errorf("the runner image %s isn't built and no runner directory is configured (agent_runner_dir)", a.image())
	}
	_, err := a.do(ctx, a.BuildArgs())
	return err
}

// verifyInternal inspects the network and refuses to go on unless it says it is internal (host-only). The
// inspect output is JSON; any object with a boolean "internal" (any case) that is true counts. Output that
// doesn't say so is a refusal too.
func (a *AppleContainer) verifyInternal(ctx context.Context) error {
	out, err := a.do(ctx, []string{a.bin(), "network", "inspect", a.network()})
	if err != nil {
		return fmt.Errorf("can't inspect network %s: %w", a.network(), err)
	}
	var v any
	if json.Unmarshal(out, &v) != nil {
		return fmt.Errorf("network %s: the inspect output isn't JSON, so it can't be shown to be internal", a.network())
	}
	if !findInternal(v) {
		return fmt.Errorf("network %s is not internal (host-only); refusing to start a VM on it", a.network())
	}
	return nil
}

func findInternal(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if strings.EqualFold(k, "internal") {
				if b, ok := val.(bool); ok {
					return b
				}
			}
		}
		for _, val := range x {
			if findInternal(val) {
				return true
			}
		}
	case []any:
		for _, val := range x {
			if findInternal(val) {
				return true
			}
		}
	}
	return false
}

// Start implements VM: image, network, container, then the runner by exec.
func (a *AppleContainer) Start(ctx context.Context, spec StartSpec) (Conn, error) {
	a.setState(StateStarting)
	fail := func(err error) (Conn, error) {
		a.setState(StateFailed)
		return nil, err
	}
	if badMountPath(spec.Project.Dir) {
		return fail(errors.New("the project path can't be mounted (a comma or control character in it)"))
	}
	if _, err := exec.LookPath(a.bin()); err != nil && a.run == nil {
		return fail(errors.New("the container tool isn't installed (https://github.com/apple/container)"))
	}
	if err := a.EnsureImage(ctx); err != nil {
		return fail(err)
	}
	if _, err := a.do(ctx, []string{a.bin(), "network", "inspect", a.network()}); err != nil {
		if _, err := a.do(ctx, a.networkCreateArgs()); err != nil {
			return fail(err)
		}
	}
	if err := a.verifyInternal(ctx); err != nil {
		return fail(err) // fail closed: a VM on a network that reaches the Internet defeats the proxy
	}
	_, _ = a.do(ctx, a.deleteArgs(spec.Project)) // a leftover of a crash
	if _, err := a.do(ctx, a.RunArgs(spec.Project)); err != nil {
		return fail(err)
	}
	a.mu.Lock()
	a.name = containerName(spec.Project)
	a.proj = spec.Project
	a.mu.Unlock()
	c, err := a.execConn(ctx, spec.Project, spec.Env, []string{"node", runnerMain})
	if err != nil {
		_ = a.Stop(context.Background())
		return fail(err)
	}
	a.setState(StateRunning)
	return c, nil
}

// Exec implements VM.
func (a *AppleContainer) Exec(ctx context.Context, argv []string, env map[string]string) (Conn, error) {
	a.mu.Lock()
	running := a.name != ""
	a.mu.Unlock()
	if !running || len(argv) == 0 {
		return nil, errors.New("the VM isn't running")
	}
	return a.execConn(ctx, a.project(), env, argv)
}

func (a *AppleContainer) project() Project {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proj
}

func (a *AppleContainer) execConn(ctx context.Context, p Project, env map[string]string, argv []string) (Conn, error) {
	a.mu.Lock()
	a.proj = p
	a.mu.Unlock()
	envFile := ""
	if len(env) > 0 {
		body, err := EnvFile(env)
		if err != nil {
			return nil, err
		}
		dir, err := os.MkdirTemp(a.EnvDir, "rubi-env-")
		if err != nil {
			return nil, err
		}
		envFile = filepath.Join(dir, "env")
		if err := os.WriteFile(envFile, body, 0o600); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
	}
	args := a.ExecArgs(p, envFile, argv)
	var c Conn
	var err error
	if a.spawn != nil {
		c, err = a.spawn(ctx, args)
	} else {
		c, err = spawnProcess(args)
	}
	if err != nil {
		removeEnv(envFile)
		return nil, err
	}
	return &envConn{Conn: c, envFile: envFile}, nil
}

// Stop implements VM.
func (a *AppleContainer) Stop(ctx context.Context) error {
	a.mu.Lock()
	name := a.name
	p := a.proj
	a.name = ""
	a.mu.Unlock()
	a.setState(StateStopped)
	if name == "" {
		return nil
	}
	_, err := a.do(ctx, a.stopArgs(p))
	_, derr := a.do(ctx, a.deleteArgs(p))
	return errors.Join(err, derr)
}

// envConn removes the environment file when the runner is up or the process ends.
type envConn struct {
	Conn
	envFile string
	once    sync.Once
}

func (e *envConn) Ready() { e.once.Do(func() { removeEnv(e.envFile) }) }
func (e *envConn) Close() error {
	e.Ready()
	return e.Conn.Close()
}

func removeEnv(file string) {
	if file != "" {
		os.RemoveAll(filepath.Dir(file))
	}
}

// procConn is a Conn on an os/exec process.
type procConn struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   io.Reader
	done  chan struct{}
}

func spawnProcess(argv []string) (Conn, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { // the runner's stderr goes to the log, line by line and bounded
		sc := bufio.NewScanner(errPipe)
		sc.Buffer(make([]byte, 0, 4096), 16<<10)
		for sc.Scan() {
			log.Printf("agent runner: %s", sc.Text())
		}
	}()
	c := &procConn{cmd: cmd, stdin: in, out: out, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

func (c *procConn) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *procConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }
func (c *procConn) CloseWrite() error           { return c.stdin.Close() }
func (c *procConn) Done() <-chan struct{}       { return c.done }
func (c *procConn) Close() error {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return nil
}
