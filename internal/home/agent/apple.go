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
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/runner"
)

// AppleContainer runs a project in a VM of Apple's container tool (github.com/apple/container, 1.6.x),
// by shelling out to its CLI. Every container gets the image below, the project at /work, a non-root
// user, no capabilities and no network at all (`--network none`: loopback only, no route to the Mac or anywhere).
// The only way out is a stream through the stdio of a long-lived `container exec -i ... rubi-netpipe` to the
// allow-list proxy and the auth gateway inside Rubi Home (pipe.go, internal/home/agent/netpipe).
type AppleContainer struct {
	// Bin is the container CLI ("container" when empty, looked up in PATH).
	Bin string
	// Image is the runner image's tag (DefaultImage when empty); RunnerDir the directory with runner/ (its
	// container/Dockerfile) to build it from when the image is missing. When RunnerDir is empty the runner
	// bundled in this binary (package runner) is written to EnvDir/runner-build/<hash> and built from there.
	Image     string
	RunnerDir string
	// CPUs and Memory limit the VM ("" = the CLI's defaults).
	CPUs   string
	Memory string
	// AuthDir is the host directory holding the sign-in shared by all projects (EnvDir/agent-auth when empty).
	// Only the sign-in container mounts it; a project VM never does (auth.go, gateway.go).
	AuthDir string
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
	pipe  *pipeSup
}

const (
	// runnerUID is the non-root user inside the image (see runner/container/Dockerfile).
	runnerUID = "10001"
	// RunnerCmd is how the image starts the runner (the exec in the VM).
	runnerMain = "/opt/rubi-runner/dist/main.js"
)

// DefaultImage is the runner image's tag: it names the bundled runner (the first 12 hex digits of its
// hash), so a Rubi Home with a changed runner builds and uses a new image.
var DefaultImage = "rubi-agent-runner:" + runner.Hash()[:12]

func (a *AppleContainer) bin() string {
	if a.Bin != "" {
		return a.Bin
	}
	return ContainerBin()
}

// ContainerBin is the container CLI: where Apple's installer puts it, else "container" in PATH (a launchd
// job's PATH doesn't have /usr/local/bin, and a PATH entry isn't trusted over the installed one).
func ContainerBin() string {
	if st, err := os.Stat(installedBin); err == nil && st.Mode().IsRegular() {
		return installedBin
	}
	if p, err := exec.LookPath("container"); err == nil {
		return p
	}
	return "container"
}

// installedBin is where Apple's installer puts the CLI; it is preferred to whatever PATH names.
var installedBin = "/usr/local/bin/container"

func (a *AppleContainer) image() string {
	if a.Image != "" {
		return a.Image
	}
	return DefaultImage
}

var nameUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// containerName is the container's id for a project: stable, so a leftover from a crash is found and
// removed by the next start. A role's carries its id (never its name, which can change) and 64 bits of a hash of
// dir|role.
func containerName(p Project) string {
	if p.Role != "" {
		sum := sha256.Sum256([]byte(p.Dir + "|" + p.Role))
		id := strings.Trim(nameUnsafe.ReplaceAllString(strings.ToLower(p.Role), "-"), "-")
		if len(id) > 24 {
			id = id[:24]
		}
		return "rubi-agent-" + id + "-" + hex.EncodeToString(sum[:8])
	}
	sum := sha256.Sum256([]byte(p.Dir))
	slug := strings.Trim(nameUnsafe.ReplaceAllString(strings.ToLower(p.ID), "-"), "-")
	if len(slug) > 24 {
		slug = slug[:24]
	}
	return "rubi-agent-" + slug + "-" + hex.EncodeToString(sum[:4])
}

// oldRoleSuffix is the end of the names the first builds gave role containers (rubi-agent-<role name>-<8 hex of
// dir|role>): the name could change, the suffix could not.
func oldRoleSuffix(p Project) string {
	sum := sha256.Sum256([]byte(p.Dir + "|" + p.Role))
	return "-" + hex.EncodeToString(sum[:4])
}

// leftoverNames are the containers of this project and role that an earlier run or an earlier naming left behind:
// the current name, and for a role the old-style names, whatever the role was called then.
func (a *AppleContainer) leftoverNames(ctx context.Context, p Project) []string {
	names := []string{containerName(p)}
	if p.Role == "" {
		return names
	}
	out, err := a.do(ctx, []string{a.bin(), "ls", "--all", "--format", "json"})
	if err != nil {
		return names
	}
	var rows []struct {
		Configuration struct {
			ID string `json:"id"`
		} `json:"configuration"`
	}
	if json.Unmarshal(out, &rows) != nil {
		return names
	}
	suffix := oldRoleSuffix(p)
	for _, r := range rows {
		if id := r.Configuration.ID; id != names[0] && strings.HasPrefix(id, "rubi-agent-") && strings.HasSuffix(id, suffix) {
			names = append(names, id)
		}
	}
	return names
}

// ImageTag is the tag of the runner image this VM runs.
func (a *AppleContainer) ImageTag() string { return a.image() }

// BuildArgs is the image build from RunnerDir: the context is the runner directory.
func (a *AppleContainer) BuildArgs() []string { return a.buildArgs(a.RunnerDir) }

func (a *AppleContainer) buildArgs(dir string) []string {
	return []string{a.bin(), "build", "--progress", "plain", "-t", a.image(),
		"-f", filepath.Join(dir, "container", "Dockerfile"), dir}
}

// BuildCommand is the image build, from RunnerDir or, when that is empty, from the bundled runner written
// to EnvDir/runner-build/<hash>.
func (a *AppleContainer) BuildCommand() ([]string, error) {
	if a.RunnerDir != "" {
		return a.BuildArgs(), nil
	}
	if a.EnvDir == "" {
		return nil, fmt.Errorf("the runner image %s isn't built and there is no directory to build it in (agent_runner_dir)", a.image())
	}
	dir, err := ExtractRunner(filepath.Join(a.EnvDir, "runner-build", runner.Hash()), runner.FS())
	if err != nil {
		return nil, err
	}
	return a.buildArgs(dir), nil
}

var extractMu sync.Mutex

// ExtractRunner writes the runner bundle to dir (0700) unless dir already holds exactly that bundle (a
// planted extra file, directory or symlink counts as a difference). It writes a private temporary directory
// beside dir and renames it into place, so a half-written copy is never used.
func ExtractRunner(dir string, bundle fs.FS) (string, error) {
	extractMu.Lock()
	defer extractMu.Unlock()
	want := runner.HashFS(bundle)
	if st, err := os.Lstat(dir); err == nil && st.IsDir() && runner.HashFS(os.DirFS(dir)) == want {
		return dir, nil
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(parent, ".extract-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp) // gone after the rename
	err = fs.WalkDir(bundle, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(tmp, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		b, err := fs.ReadFile(bundle, p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	})
	if err != nil {
		return "", fmt.Errorf("writing the runner sources: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// ImageExists says whether the runner image is there.
func (a *AppleContainer) ImageExists(ctx context.Context) bool {
	_, err := a.do(ctx, a.imageInspectArgs())
	return err == nil
}

func (a *AppleContainer) imageInspectArgs() []string {
	return []string{a.bin(), "image", "inspect", a.image()}
}

// RunArgs starts the VM detached: the project at /work, no network, no capabilities, a
// read-only root with a tmpfs for /tmp, a non-root user. The container just waits; the runner is started by
// exec so its environment (the run token) never appears on a command line.
func (a *AppleContainer) RunArgs(p Project) []string {
	args := []string{a.bin(), "run", "-d", "--name", containerName(p), "--init",
		"--network", "none", "--user", runnerUID, "--cap-drop", "ALL", "--read-only",
		"--tmpfs", "/tmp",
		"--mount", "type=bind,source=" + p.Dir + ",target=/work"}
	if p.Role != "" {
		// A role VM sees its own content (read-only) and its own config directory, nothing of any other role, no
		// sign-in (the auth gateway holds it) and never the user's SSH agent (no --ssh). The legacy role's state
		// (/work/.rubi) is covered by an empty read-only directory and the project's own .claude is read-only, so a
		// role can't rewrite what the other roles load. These binds sit on top of the read-write one of /work.
		if p.EmptyDir != "" {
			args = append(args, "--mount", "type=bind,source="+p.EmptyDir+",target=/work/.rubi,readonly")
		}
		for _, name := range p.ReadOnly {
			args = append(args, "--mount", "type=bind,source="+filepath.Join(p.Dir, name)+",target=/work/"+name+",readonly")
		}
		args = append(args,
			"--mount", "type=bind,source="+p.RoleDir+",target="+RoleMountDir+",readonly",
			"--mount", "type=bind,source="+p.ClaudeDir+",target="+RoleClaudeDir,
			"--env", "RUBI_ROLE_DIR="+RoleMountDir, "--env", "RUBI_CLAUDE_DIR="+RoleClaudeDir)
	}
	args = append(args, "--workdir", "/work")
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
	return a.execArgs(containerName(p), envFile, argv)
}

func (a *AppleContainer) execArgs(name, envFile string, argv []string) []string {
	args := []string{a.bin(), "exec", "-i", "--user", runnerUID, "--workdir", "/work"}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	args = append(args, name)
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
	argv, err := a.BuildCommand()
	if err != nil {
		return err
	}
	_, err = a.do(ctx, argv)
	return err
}

// Netless implements NetlessVM: the VM has no network, only the pipe.
func (a *AppleContainer) Netless() bool { return true }

// Start implements VM: image, container (no network), the pipe, then the runner by exec.
func (a *AppleContainer) Start(ctx context.Context, spec StartSpec) (Conn, error) {
	a.setState(StateStarting)
	fail := func(err error) (Conn, error) {
		a.setState(StateFailed)
		return nil, err
	}
	bad := badMountPath(spec.Project.Dir) || badMountPath(spec.Project.RoleDir) || badMountPath(spec.Project.ClaudeDir) || badMountPath(spec.Project.EmptyDir)
	for _, n := range spec.Project.ReadOnly {
		bad = bad || badMountPath(n)
	}
	if bad {
		return fail(errors.New("the project path can't be mounted (a comma or control character in it)"))
	}
	if r := spec.Project.Role; r != "" && (spec.Project.RoleDir == "" || spec.Project.ClaudeDir == "") {
		return fail(fmt.Errorf("role %s has no directories to mount", r))
	} else if r != "" && spec.Project.EmptyDir == "" { // fail closed: a role never gets the legacy role's state read-write
		return fail(fmt.Errorf("role %s has no empty directory to cover /work/.rubi with", r))
	}
	if _, err := exec.LookPath(a.bin()); err != nil && a.run == nil {
		return fail(errors.New("the container tool isn't installed (https://github.com/apple/container)"))
	}
	if err := a.EnsureImage(ctx); err != nil {
		return fail(err)
	}
	if spec.Pipe == nil {
		return fail(errors.New("a VM without a network needs its pipe")) // fail closed: nothing would serve it
	}
	for _, n := range a.leftoverNames(ctx, spec.Project) { // a leftover of a crash, or of an older name
		_, _ = a.do(ctx, []string{a.bin(), "delete", "--force", n})
	}
	if _, err := a.do(ctx, a.RunArgs(spec.Project)); err != nil {
		return fail(err)
	}
	a.mu.Lock()
	a.name = containerName(spec.Project)
	a.proj = spec.Project
	a.mu.Unlock()
	pipe, err := a.startPipe(ctx, containerName(spec.Project), spec.Pipe, func() {
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = a.Stop(sctx)
	})
	if err != nil {
		_ = a.Stop(context.Background())
		return fail(fmt.Errorf("the network pipe: %w", err))
	}
	a.mu.Lock()
	a.pipe = pipe
	a.mu.Unlock()
	env, err := prepareEnv(ctx, spec, "")
	if err == nil && spec.Project.Role != "" { // the exec doesn't depend on the container's own environment
		env = withRoleEnv(env)
	}
	if err != nil {
		_ = a.Stop(context.Background())
		return fail(err)
	}
	c, err := a.execConn(ctx, spec.Project, env, []string{"node", runnerMain}, true)
	if err != nil {
		_ = a.Stop(context.Background())
		return fail(err)
	}
	a.setState(StateRunning)
	return c, nil
}

func withRoleEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+2)
	for k, v := range env {
		out[k] = v
	}
	out["RUBI_ROLE_DIR"], out["RUBI_CLAUDE_DIR"] = RoleMountDir, RoleClaudeDir
	return out
}

// Exec implements VM.
func (a *AppleContainer) Exec(ctx context.Context, argv []string, env map[string]string) (Conn, error) {
	a.mu.Lock()
	running := a.name != ""
	a.mu.Unlock()
	if !running || len(argv) == 0 {
		return nil, errors.New("the VM isn't running")
	}
	return a.execConn(ctx, a.project(), env, argv, false) // its stderr is dropped: a sign-in may print secrets
}

func (a *AppleContainer) project() Project {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proj
}

// execConn starts argv in the container. logStderr sends the process's stderr to the log (the runner's, line
// by line and without lines that look secret); otherwise it is discarded.
func (a *AppleContainer) execConn(ctx context.Context, p Project, env map[string]string, argv []string, logStderr bool) (Conn, error) {
	a.mu.Lock()
	a.proj = p
	a.mu.Unlock()
	return a.execNamed(ctx, containerName(p), env, argv, logStderr)
}

// execNamed is execConn for any container by name.
func (a *AppleContainer) execNamed(ctx context.Context, name string, env map[string]string, argv []string, logStderr bool) (Conn, error) {
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
	args := a.execArgs(name, envFile, argv)
	var c Conn
	var err error
	if a.spawn != nil {
		c, err = a.spawn(ctx, args)
	} else {
		c, err = spawnProcess(args, logStderr)
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
	pipe := a.pipe
	a.pipe = nil
	a.mu.Unlock()
	pipe.stop()
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

func spawnProcess(argv []string, logStderr bool) (Conn, error) {
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
	go func() { // the runner's stderr goes to the log, line by line and bounded; other processes' is dropped
		if !logStderr {
			_, _ = io.Copy(io.Discard, errPipe)
			return
		}
		sc := bufio.NewScanner(errPipe)
		sc.Buffer(make([]byte, 0, 4096), 16<<10)
		for sc.Scan() {
			if !LooksSecret(sc.Text()) {
				log.Printf("agent runner: %s", sc.Text())
			}
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
