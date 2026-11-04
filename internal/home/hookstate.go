package home

// The Claude Code hook binary as the panel sees it: the state `hello` reports, and the `hook.install` op that
// installs or updates the root-owned copy through the macOS administrator dialog (no terminal, no sudo).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	claudeplugin "github.com/Deikus-LXXVII/rubi/claude-plugin"
	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

const (
	hookInstallLimit = 5 * time.Minute  // how long the administrator dialog may stay open
	hookNoAgentFor   = 5 * time.Minute  // how long "nobody to ask" is believed
	hookCooldown     = 2 * time.Minute  // no new installation this long after a dialog nobody answered
	hookCacheTTL     = 10 * time.Second // how long an inspection of the hook file is reused
)

// Results of an installation, as `hello.hook.last` reports them.
const (
	HookResultOK        = "ok"
	HookResultCancelled = "cancelled"
	HookResultTimeout   = "timeout"
	HookResultFailed    = "failed"
)

// Hook states in `hello`.
const (
	HookMissing  = "missing"  // nothing at the hook path
	HookCurrent  = "current"  // root-owned, and not older than this Rubi Home
	HookStale    = "stale"    // older than this Rubi Home, or its version can't be read
	HookInsecure = "insecure" // not root-owned, writable by others, a symlink, or in an unsafe directory
	HookDev      = "dev"      // this Rubi Home or the hook is a development build: versions can't be compared
)

// RunningExecutable is this binary's real path (symlinks resolved), the source of an install.
func RunningExecutable() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	return exe, err
}

// hookTarget is the one file hook.install writes and `hello` inspects: DefaultHookBinary, never
// $RUBI_HOOK_BINARY (that variable only points `rubi-home status` somewhere else). hookPath is the test seam.
func (h *Helper) hookTarget() string {
	if h.hookPath != "" {
		return h.hookPath
	}
	return DefaultHookBinary
}

func (h *Helper) clock() time.Time {
	if h.hookClock != nil {
		return h.hookClock()
	}
	return time.Now()
}

func (h *Helper) pluginTarget() string {
	if h.pluginAt != "" {
		return h.pluginAt
	}
	return DefaultClaudePluginDir
}

func (h *Helper) pluginInstalled() bool {
	fi, err := os.Lstat(h.pluginTarget())
	return err == nil && fi.IsDir()
}

// hookStateOf names the state of an inspected hook binary; self is this Rubi Home's version.
func hookStateOf(b HookBinary, self string) string {
	switch {
	case b.Missing:
		return HookMissing
	case !b.Secure:
		return HookInsecure
	case b.VersionE != "" || b.Version == "":
		return HookStale
	case versionOlder(b.Version, self):
		return HookStale
	}
	_, _, selfOK := semver(self)
	_, _, hookOK := semver(b.Version)
	if !selfOK || !hookOK {
		return HookDev
	}
	return HookCurrent
}

// hookResult is how the last installation ended.
type hookResult struct {
	Result  string // ok, cancelled, timeout or failed
	Code    string // the reason code of a failure ("" otherwise)
	Message string // a short sentence for the user, with no path in it
	At      time.Time
}

// hookSentence is the fixed sentence for a reason code. Paths and system errors stay in the log.
func hookSentence(code string) string {
	switch code {
	case CodeBadPath:
		return "the hook's location on that computer can't be used; see Rubi Home's log"
	case CodeNoSource:
		return "Rubi Home can't read its own file to install it; see Rubi Home's log"
	case CodeNotDir:
		return "the hook's folder on that computer isn't a folder; see Rubi Home's log"
	case CodeNotPlain:
		return "the hook on that computer is a link or something else, not a file; remove it there and try again"
	case CodeUnsafeDir:
		return "the hook's folder on that computer isn't protected; see Rubi Home's log"
	case CodeSelf:
		return "the hook is Rubi Home's own file; see Rubi Home's log"
	case CodePlugin:
		return "the Claude Code plugin's folder on that computer can't be used; see Rubi Home's log"
	case CodeVerify:
		return "the installed hook didn't check out; see Rubi Home's log"
	case CodeCooldown:
		return "the administrator dialog went unanswered a moment ago; try again in 2 minutes"
	case CodeUpToDate:
		return "the hook is up to date already"
	case CodeBusy:
		return "the hook is being installed already; answer the administrator dialog on that computer"
	case CodeBadRequest:
		return "bad arguments"
	case CodePlanChanged:
		return "what would be installed is not what was approved; ask for the approval again"
	case CodePlanNeeded:
		return "the installation needs the hash of the approved plan; ask for the approval again"
	case CodeInUse:
		return "a Claude Code hook is running; close Claude Code and try again"
	}
	return "the hook could not be installed; see Rubi Home's log"
}

// refusal is the error a refused op gives core: the fixed sentence and the code, nothing from the system.
func refusal(err error) error {
	code := codeOf(err)
	if code == "" {
		code = "failed"
	}
	return errors.New(hookSentence(code) + " (" + code + ")")
}

// hookFileKey is what tells that the hook file did not change since it was inspected.
type hookFileKey struct {
	mtime, size int64
	ino         uint64
	uid         uint32
	mode        os.FileMode
}

type hookKey struct{ link, file hookFileKey }

func fileKeyOf(fi os.FileInfo) hookFileKey {
	k := hookFileKey{mtime: fi.ModTime().UnixNano(), size: fi.Size(), mode: fi.Mode()}
	if uid, ino, ok := fsx.OwnerOf(fi); ok {
		k.ino, k.uid = ino, uid
	}
	return k
}

func hookKeyOf(path string) (hookKey, bool) {
	li, err := os.Lstat(path)
	if err != nil {
		return hookKey{}, false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return hookKey{}, false
	}
	return hookKey{fileKeyOf(li), fileKeyOf(fi)}, true
}

// hookCached is the last inspection of the hook file, keyed on its mtime and size (and owner, mode, inode).
type hookCached struct {
	path string
	key  hookKey
	at   time.Time
	b    HookBinary
	ok   bool
}

// inspectTarget looks at the hook file for `hello` and the preconditions: no SHA-256, and `version` only runs
// on a file that is Secure. A result is reused for hookCacheTTL while the file looks the same; fresh skips that.
func (h *Helper) inspectTarget(fresh bool) HookBinary {
	path := h.hookTarget()
	key, keyed := hookKeyOf(path)
	if keyed && !fresh {
		h.hookMu.Lock()
		c := h.hookSeen
		h.hookMu.Unlock()
		if c.ok && c.path == path && c.key == key && h.clock().Sub(c.at) < hookCacheTTL {
			return c.b
		}
	}
	b := inspectHook(path, HookBinaryTimeout, inspectOpts{afterSecure: h.hookTweak})
	if keyed {
		h.hookMu.Lock()
		h.hookSeen = hookCached{path: path, key: key, at: h.clock(), b: b, ok: true}
		h.hookMu.Unlock()
	}
	return b
}

// whyInsecure names what is wrong with an insecure hook: symlink, owner (the file's owner or mode) or dir.
func whyInsecure(b HookBinary) string {
	switch {
	case b.Symlink:
		return "symlink"
	case b.WhyInsecure != "":
		return "dir"
	}
	return "owner"
}

// installOptions are what an installation uses; the Claude Code plugin comes along when it is installed
// already, or when asked for.
func (h *Helper) installOptions(withPlugin bool) (InstallOptions, error) {
	src, err := RunningExecutable()
	if err != nil {
		return InstallOptions{}, &coded{CodeNoSource, err}
	}
	o := InstallOptions{Src: src, Dst: h.hookTarget(), RequireRoot: true,
		WithPlugin: withPlugin || h.pluginInstalled(), PluginDst: h.pluginTarget()}
	if h.hookOpts != nil {
		h.hookOpts(&o)
	}
	return o, nil
}

// hookFixable says whether an installation could work at all, from planInstall's own checks (no hash, no
// script, no prompt).
func (h *Helper) hookFixable() bool {
	o, err := h.installOptions(false)
	return err == nil && checkInstall(o.Src, o.Dst, o.RequireRoot) == nil
}

// hookStatus is the `hook` part of `hello`. It never fails: whatever goes wrong is a state.
func (h *Helper) hookStatus() map[string]any {
	b := h.inspectTarget(false)
	state := hookStateOf(b, helperVersion())
	out := map[string]any{"state": state, "version": shownVersion(b.Version), "plugin": h.pluginInstalled()}
	h.hookMu.Lock()
	out["installing"] = h.hookBusy
	if l := h.hookLast; l != nil {
		last := map[string]any{"result": l.Result, "message": l.Message, "at": l.At.Unix()}
		if l.Code != "" {
			last["code"] = l.Code
		}
		out["last"] = last
	}
	h.hookMu.Unlock()
	if state == HookInsecure {
		out["why"] = whyInsecure(b)
	}
	if state == HookMissing || state == HookStale || state == HookInsecure {
		out["fixable"] = h.hookFixable()
	}
	return out
}

type hookArgs struct {
	WithPlugin bool `json:"with_plugin"`
	// PlanSHA256 is the hash of the plan the person approved (hook.plan gave it): when set, hook.install runs only a plan
	// with exactly that hash.
	PlanSHA256 string `json:"plan_sha256"`
}

// elevator is how the administrator is asked on this computer.
func (h *Helper) elevator() Elevator {
	if h.hookElev != nil {
		return h.hookElev
	}
	return NewElevator(ElevateEnv{})
}

// fullPlan is the whole plan hook.install would run: the hook binary and, with o.WithPlugin, the Claude Code plugin.
func fullPlan(o InstallOptions) (InstallPlan, error) {
	tools := defaultTools()
	if o.tools != nil {
		tools = *o.tools
	}
	p, err := planInstall(o.Src, o.Dst, o.RequireRoot, tools)
	if err != nil {
		return p, err
	}
	if o.WithPlugin {
		dst := o.PluginDst
		if dst == "" {
			dst = DefaultClaudePluginDir
		}
		pp, err := planClaudePlugin(claudeplugin.Files, dst, o.RequireRoot, tools)
		if err != nil {
			return p, &coded{CodePlugin, err}
		}
		p.Plugin = &pp
	}
	return p, nil
}

func parseHookArgs(args json.RawMessage) (hookArgs, error) {
	var in hookArgs
	if len(args) > 0 && string(args) != "null" && json.Unmarshal(args, &in) != nil {
		return in, &coded{CodeBadRequest, errors.New("bad arguments")}
	}
	return in, nil
}

// hookRefusedNow is the refusal for an installation that must not start now (hookMu held).
func (h *Helper) hookRefusedNow() error {
	if h.hookBusy {
		return &coded{CodeBusy, errors.New("an installation is running")}
	}
	if l := h.hookLast; l != nil && (l.Result == HookResultCancelled || l.Result == HookResultTimeout) && h.clock().Sub(l.At) < hookCooldown {
		return &coded{CodeCooldown, errors.New("the last administrator dialog got no answer")}
	}
	return nil
}

// hookPreflight runs every check that does not need the administrator: nothing is changed and nothing is
// asked. It refuses what is running or was refused a moment ago, what is up to date, and what could not work.
func (h *Helper) hookPreflight(withPlugin bool) (InstallPlan, InstallOptions, error) {
	h.hookMu.Lock()
	err := h.hookRefusedNow()
	h.hookMu.Unlock()
	if err != nil {
		return InstallPlan{}, InstallOptions{}, err
	}
	o, err := h.installOptions(withPlugin)
	if err != nil {
		return InstallPlan{}, o, err
	}
	state := hookStateOf(h.inspectTarget(true), helperVersion())
	pluginNeeded := false
	if o.WithPlugin {
		pp, err := planClaudePlugin(claudeplugin.Files, o.PluginDst, o.RequireRoot, defaultTools())
		if err != nil {
			return InstallPlan{}, o, &coded{CodePlugin, err}
		}
		pluginNeeded = !h.pluginInstalled() || verifyPlugin(pp, false) != nil
	}
	if state == HookCurrent && !pluginNeeded {
		return InstallPlan{}, o, &coded{CodeUpToDate, errors.New("nothing to install")}
	}
	p, err := planInstall(o.Src, o.Dst, o.RequireRoot, defaultTools())
	return p, o, err
}

// planHook is the `hook.plan` op: what hook.install would put where, without asking anyone anything.
// Core shows it in the approval.
func (h *Helper) planHook(args json.RawMessage) (any, error) {
	in, err := parseHookArgs(args)
	if err != nil {
		return nil, refusal(err)
	}
	_, o, err := h.hookPreflight(in.WithPlugin)
	if err != nil {
		log.Printf("hook.plan: %v", err)
		return nil, refusal(err)
	}
	p, err := fullPlan(o)
	if err != nil {
		log.Printf("hook.plan: %v", err)
		return nil, refusal(err)
	}
	// plan_sha256 is the hash of everything that would run as administrator (the binary's hash, the paths, the plugin's
	// files). The approval shows it and hook.install is told it; "admin" says what the person will be asked.
	return map[string]any{"target": p.Dst, "version": p.Version, "sha256": p.SHA256, "plan_sha256": PlanSHA256(p),
		"admin": h.elevator().Sentence(), "plugin": p.Plugin != nil}, nil
}

// installHook is the `hook.install` op. Args: {"with_plugin": bool}. It checks what it can, starts the
// installation in the background (the administrator dialog may stay open for minutes, longer than a request
// lives) and answers at once with {"started": true}; `hello.hook` tells when it ends. Only one runs at a time.
func (h *Helper) installHook(args json.RawMessage) (any, error) {
	in, err := parseHookArgs(args)
	if err != nil {
		return nil, refusal(err)
	}
	_, o, err := h.hookPreflight(in.WithPlugin)
	if err != nil {
		log.Printf("hook.install: refused: %v", err)
		return nil, refusal(err)
	}
	// The plan is made again here and compared with the one that was approved: a binary or a file swapped since the
	// approval changes the hash, and nothing is run.
	plan, err := fullPlan(o)
	if err != nil {
		log.Printf("hook.install: refused: %v", err)
		return nil, refusal(err)
	}
	if in.PlanSHA256 == "" && o.Run == nil {
		// Without the hash of the approved plan there is nothing to hold the plan to: the answer might be a command for the
		// Bot, and the person approved a hash, not "whatever Home makes now".
		log.Printf("hook.install: refused: no plan hash")
		return nil, refusal(&coded{CodePlanNeeded, errors.New("no plan hash")})
	}
	if in.PlanSHA256 != "" && strings.ToLower(in.PlanSHA256) != PlanSHA256(plan) {
		log.Printf("hook.install: refused: the plan is not the approved one")
		return nil, refusal(&coded{CodePlanChanged, errors.New("the plan changed")})
	}
	elev := h.elevator()
	o.Elevator = elev
	h.hookMu.Lock()
	// "Nobody to ask" is remembered for a few minutes only: a polkit agent can start, a session can begin.
	noAgent := !h.hookNoAgentAt.IsZero() && h.clock().Sub(h.hookNoAgentAt) < hookNoAgentFor
	h.hookMu.Unlock()
	if aerr := elev.Available(context.Background()); o.Run == nil && (noAgent || errors.Is(aerr, ErrNoAgent)) {
		// Nobody to ask for the administrator password on this computer: the person's approval in the panel stands in for
		// it, and the Bot runs one command as root. Home runs nothing itself.
		return h.botSudoAnswer(o, plan), nil
	}
	h.hookMu.Lock()
	if err := h.hookRefusedNow(); err != nil {
		h.hookMu.Unlock()
		return nil, refusal(err)
	}
	h.hookBusy, h.hookLast = true, nil
	h.hookMu.Unlock()
	limit := h.hookLimit
	if limit <= 0 {
		limit = hookInstallLimit
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), limit) // its own: not the request's
		defer cancel()
		o.ExpectPlanSHA256 = PlanSHA256(plan) // the plan InstallHook makes must be the one just checked
		err := InstallHook(ctx, io.Discard, o)
		res := hookResultOf(ctx, err)
		h.hookMu.Lock()
		res.At = h.clock()
		if errors.Is(err, ErrNoAgent) {
			h.hookNoAgentAt = res.At // the next request goes the headless way
		}
		h.hookBusy, h.hookLast, h.hookSeen = false, &res, hookCached{}
		h.hookMu.Unlock()
		if err != nil {
			log.Printf("hook.install: %s: %v", res.Result, err)
		}
	}()
	return map[string]any{"started": true, "version": version.Version}, nil
}

// hookResultOf names how an installation ended. A dialog killed by the time limit is a timeout, not the
// "signal: killed" osascript reports.
func hookResultOf(ctx context.Context, err error) hookResult {
	switch {
	case err == nil:
		return hookResult{Result: HookResultOK, Message: "The Claude Code hook is installed."}
	case errors.Is(err, ErrAdminCancelled):
		return hookResult{Result: HookResultCancelled, Message: "The administrator dialog was cancelled."}
	case errors.Is(err, ErrNoAgent):
		return hookResult{Result: HookResultFailed, Code: "no_agent", Message: "There was nobody to ask for the administrator password; your Bot can install it."}
	case ctx.Err() != nil:
		return hookResult{Result: HookResultTimeout, Message: "The administrator dialog timed out; try again."}
	}
	code := codeOf(err)
	return hookResult{Result: HookResultFailed, Code: code, Message: capitalize(hookSentence(code)) + "."}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 'a' - 'A'
	}
	return string(r)
}

// sudoCommand is the headless command: `/usr/bin/sudo -- /bin/sh -c SCRIPT`, by absolute paths, with the plan's own script as the
// one argument. It is what pkexec runs, and it is checkable: sha256(SCRIPT) is the plan hash the person approved.
func sudoCommand(p InstallPlan) []string {
	return []string{"/usr/bin/sudo", "--", "/bin/sh", "-c", p.FullScript()}
}

// botSudoAnswer is hook.install's answer where nobody can be asked for the administrator password (no polkit agent, no
// desktop): the exact argument list for the Bot to run, and the hash of the script in it. Rubi Home runs nothing itself and
// its own binary is never run as root, so no file the user can write is ever executed with root's rights. The script reads
// the binary to install once, checks the SHA-256 of that copy against the approved plan, uses absolute tools, checks that
// the folders above its target are root's, and replaces only a file called rubi-home and a folder called claude-plugin that is
// the Rubi plugin. A Bot that runs anything else with sudo gets nothing from this.
func (h *Helper) botSudoAnswer(o InstallOptions, plan InstallPlan) map[string]any {
	return map[string]any{"started": false, "needs_bot_sudo": true, "plan_sha256": PlanSHA256(plan), "argv": sudoCommand(plan),
		"version": version.Version,
		"note":    "No one can be asked for the administrator password on this computer. Run the argument list in argv as it is (no shell around it, nothing added), and nothing else with administrator rights. You may check first that the SHA-256 of the last element of argv is plan_sha256, the hash the user approved."}
}
