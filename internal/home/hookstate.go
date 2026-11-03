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
	"syscall"
	"time"

	claudeplugin "github.com/Deikus-LXXVII/rubi/claude-plugin"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

const (
	hookInstallLimit = 5 * time.Minute  // how long the administrator dialog may stay open
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
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		k.ino, k.uid = st.Ino, st.Uid
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
	p, _, err := h.hookPreflight(in.WithPlugin)
	if err != nil {
		log.Printf("hook.plan: %v", err)
		return nil, refusal(err)
	}
	return map[string]any{"target": p.Dst, "version": p.Version, "sha256": p.SHA256}, nil
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
		err := InstallHook(ctx, io.Discard, o)
		res := hookResultOf(ctx, err)
		h.hookMu.Lock()
		res.At = h.clock()
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
