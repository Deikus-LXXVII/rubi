package home

// `rubi-home install-hook`: copies this binary to the root-owned hook path with administrator rights.
// The privileged step runs through a seam (InstallRunner), so tests copy to a temporary path instead.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	claudeplugin "github.com/Deikus-LXXVII/rubi/claude-plugin"
	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

// InstallPlan is what install-hook is about to do.
type InstallPlan struct {
	Src, Dst string
	SHA256   string // of Src
	Version  string
	Group    string // wheel on macOS
	Script   string // the shell script root runs (see installScript)
	// Plugin is the Claude Code marketplace to install in the same run (nil without WithPlugin).
	Plugin *PluginPlan
}

// FullScript is everything root runs: the binary's script, then the plugin's in a subshell (its own trap).
func (p InstallPlan) FullScript() string {
	if p.Plugin == nil {
		return p.Script
	}
	return p.Script + "\n(\n" + p.Plugin.Script + ")\n"
}

// InstallRunner runs the plan's privileged script.
type InstallRunner func(ctx context.Context, p InstallPlan) error

// InstallOptions are install-hook's inputs.
type InstallOptions struct {
	Src, Dst string
	DryRun   bool
	// Run is the privileged runner; nil uses osascript (the macOS administrator dialog).
	Run InstallRunner
	// RequireRoot demands root-owned, not group/world-writable ancestors of the target and a root-owned
	// result, and puts the same checks and the chown into the script. Tests that install to a temporary
	// path without root turn it off.
	RequireRoot bool
	// WithPlugin also installs the embedded Claude Code marketplace to PluginDst (DefaultClaudePluginDir when
	// empty) in the same privileged run.
	WithPlugin bool
	PluginDst  string
	// VersionTimeout bounds `dst version` (HookBinaryTimeout when zero).
	VersionTimeout time.Duration
	tools          *installTools // a test seam: the tools the script uses
}

// privilegedPath is the only PATH the privileged commands get (and every tool in the script is absolute).
const privilegedPath = "PATH=/usr/bin:/bin:/usr/sbin:/sbin"

// installTools are the absolute paths of the tools the script uses.
type installTools struct {
	Stat, Cat, Sha, Cut, Dirname, Mktemp, Rm, Chown, Chmod, Mv string
	StatFmt                                                    string // stat's format option and string
	Group                                                      string
	Mkdir, B64D                                                string // mkdir, and base64 with its decode option
}

func defaultTools() installTools {
	if runtime.GOOS == "darwin" {
		return installTools{Stat: "/usr/bin/stat -L -f '%u:%Lp'", Cat: "/bin/cat", Sha: "/usr/bin/shasum -a 256",
			Cut: "/usr/bin/cut", Dirname: "/usr/bin/dirname", Mktemp: "/usr/bin/mktemp", Rm: "/bin/rm",
			Chown: "/usr/sbin/chown", Chmod: "/bin/chmod", Mv: "/bin/mv", Group: "wheel",
			Mkdir: "/bin/mkdir", B64D: "/usr/bin/base64 -D"}
	}
	return installTools{Stat: "/usr/bin/stat -L -c '%u:%a'", Cat: "/bin/cat", Sha: "/usr/bin/sha256sum",
		Cut: "/usr/bin/cut", Dirname: "/usr/bin/dirname", Mktemp: "/usr/bin/mktemp", Rm: "/bin/rm",
		Chown: "/bin/chown", Chmod: "/bin/chmod", Mv: "/bin/mv", Group: "root",
		Mkdir: "/bin/mkdir", B64D: "/usr/bin/base64 -d"}
}

// installScript is what root runs. The source is read once, as root, into a temporary file in the target's
// directory; root hashes that copy and goes on only when it equals want (the hash the user was shown), then
// makes it root-owned, mode 0755, and renames it over the target (atomic). Nothing the user can write to is
// read twice or used after the check. The temporary file is removed on any failure. With asRoot the script
// also demands that the directory and all its ancestors are root-owned and not group/world-writable; a missing
// directory is created (mkdir -p) after its nearest existing ancestor passed that check. Every
// path in it is single-quoted; unsafePath keeps quotes out of them.
func installScript(t installTools, src, dst, want string, asRoot bool) string {
	dir := filepath.Dir(dst)
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	w("set -e; umask 022")
	w("src='%s'; dst='%s'; dir='%s'; want='%s'", src, dst, dir, want)
	if asRoot {
		// chk demands that a directory and all its ancestors are root-owned and not group/world-writable.
		w("chk() { d=$1; while :; do")
		w("  s=$(%s \"$d\") || exit 1", t.Stat)
		w("  [ \"${s%%%%:*}\" = 0 ] || { echo \"$d is not owned by root\" >&2; exit 1; }")
		w("  [ $((0${s#*:} & 022)) -eq 0 ] || { echo \"$d is writable by group or others\" >&2; exit 1; }")
		w("  [ \"$d\" = / ] && break")
		w("  d=$(%s \"$d\")", t.Dirname)
		w("done; }")
	}
	// A missing directory is made here, as root: the nearest existing ancestor is checked first, so nothing
	// is created below a directory anyone else could write to.
	w("if [ ! -d \"$dir\" ]; then")
	if asRoot {
		w("  n=$dir; while [ ! -d \"$n\" ]; do n=$(%s \"$n\"); done; chk \"$n\"", t.Dirname)
	}
	w("  %s -p \"$dir\"", t.Mkdir)
	w("fi")
	if asRoot {
		w("chk \"$dir\"")
	}
	w("tmp=$(%s \"$dir/.rubi-home.XXXXXX\")", t.Mktemp)
	w("trap '%s -f \"$tmp\"' EXIT", t.Rm)
	w("%s \"$src\" > \"$tmp\"", t.Cat)
	w("[ \"$(%s < \"$tmp\" | %s -d' ' -f1)\" = \"$want\" ] || { echo \"the file read from $src is not the one with sha256 $want; nothing installed\" >&2; exit 1; }", t.Sha, t.Cut)
	if asRoot {
		w("%s root:%s \"$tmp\"", t.Chown, t.Group)
	}
	w("%s 0755 \"$tmp\"", t.Chmod)
	w("%s -f \"$tmp\" \"$dst\"", t.Mv)
	return b.String()
}

// unsafePath reports why a path can't be put into the privileged script, "" when it can. Paths with
// quotes, backslashes, control or invisible characters are refused, not escaped.
func unsafePath(p string) string {
	if !filepath.IsAbs(p) {
		return "it isn't an absolute path"
	}
	if !utf8.ValidString(p) {
		return "it isn't valid UTF-8"
	}
	for _, r := range p {
		if r == '"' || r == '\'' || r == '\\' || r == '`' || r == '$' || unicode.IsControl(r) ||
			unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return fmt.Sprintf("it contains %q, which can't be quoted safely for the administrator prompt", r)
		}
	}
	return ""
}

// PlanInstallHook checks the paths and builds the plan (it reads Src; it changes nothing).
func PlanInstallHook(src, dst string, requireRoot bool) (InstallPlan, error) {
	return planInstall(src, dst, requireRoot, defaultTools())
}

// Reason codes of a refused installation. The detail (paths) goes to the log and the command line only; the
// panel and core get the code and a fixed sentence (see hookSentence).
const (
	CodeBadPath    = "bad_path"    // a path can't be quoted safely
	CodeNoSource   = "no_source"   // the file to install can't be read
	CodeNotDir     = "not_dir"     // the hook's folder is something else
	CodeNotPlain   = "not_plain"   // the hook is a symlink or something else
	CodeUnsafeDir  = "unsafe_dir"  // a folder above the hook isn't root's
	CodeSelf       = "self"        // the target is the running binary itself
	CodePlugin     = "plugin"      // the plugin's target can't be used
	CodeVerify     = "verify"      // the installed file isn't what was sent
	CodeCooldown   = "cooldown"    // asked again too soon after a dialog nobody answered
	CodeUpToDate   = "up_to_date"  // nothing to install
	CodeBusy       = "busy"        // an installation is running
	CodeBadRequest = "bad_request" // unreadable arguments
)

// coded is an error with a reason code. Its Error() is the detailed text for the log and the command line.
type coded struct {
	Code string
	Err  error
}

func (e *coded) Error() string { return e.Err.Error() }
func (e *coded) Unwrap() error { return e.Err }

func codedf(code, format string, a ...any) error { return &coded{code, fmt.Errorf(format, a...)} }

// codeOf is the reason code of err ("" when it has none).
func codeOf(err error) string {
	var c *coded
	if errors.As(err, &c) {
		return c.Code
	}
	return ""
}

func planInstall(src, dst string, requireRoot bool, tools installTools) (InstallPlan, error) {
	if err := checkInstall(src, dst, requireRoot); err != nil {
		return InstallPlan{}, err
	}
	sum, err := fileSHA256(src)
	if err != nil {
		return InstallPlan{}, codedf(CodeNoSource, "couldn't read %s: %v", src, err)
	}
	return InstallPlan{Src: src, Dst: dst, SHA256: sum, Version: version.Version, Group: tools.Group,
		Script: installScript(tools, src, dst, sum, requireRoot)}, nil
}

// checkInstall is planInstall's checks without the hash and the script: what decides whether the install can
// work at all (the panel asks it to know whether an insecure hook is fixable).
func checkInstall(src, dst string, requireRoot bool) error {
	for _, x := range []struct{ what, p string }{{"the source", src}, {"the target", dst}} {
		if why := unsafePath(x.p); why != "" {
			return codedf(CodeBadPath, "%s path %q is refused: %s", x.what, x.p, why)
		}
	}
	fi, err := os.Stat(src)
	if err != nil || !fi.Mode().IsRegular() {
		return codedf(CodeNoSource, "the source %s isn't a readable file", src)
	}
	dir := filepath.Dir(dst)
	if st, err := os.Stat(dir); err == nil && !st.IsDir() {
		return codedf(CodeNotDir, "%s exists and isn't a directory", dir)
	}
	if li, err := os.Lstat(dst); err == nil && !li.Mode().IsRegular() {
		return codedf(CodeNotPlain, "%s exists and isn't a plain file (a symlink or something else); remove it by hand and try again", dst)
	}
	if requireRoot {
		probe := dst // a missing directory is made by the script: the nearest one that exists decides
		for {
			if _, err := os.Stat(filepath.Dir(probe)); err == nil {
				break
			}
			if probe = filepath.Dir(probe); probe == "/" || probe == "." {
				break
			}
		}
		if why := insecureAncestors(probe); why != "" {
			return codedf(CodeUnsafeDir, "refusing to install into %s: %s. A hook binary is only safe where no one but root can replace it or its directories", dir, why)
		}
	}
	if rs, e1 := filepath.EvalSymlinks(src); e1 == nil {
		if rd, e2 := filepath.EvalSymlinks(dst); e2 == nil && rs == rd {
			return codedf(CodeSelf, "%s is this binary itself; install from the helper copy (%s)", dst, "~/.rubi-home/bin/rubi-home")
		}
	}
	return nil
}

// privilegedEnv is the scrubbed environment for osascript.
func privilegedEnv() []string { return []string{privilegedPath} }

// ErrAdminCancelled is what a refused or closed macOS administrator dialog gives.
var ErrAdminCancelled = errors.New("the administrator dialog was cancelled")

// adminPrompt names the request in the macOS administrator dialog itself.
const adminPrompt = "Rubi Home wants to install the Claude Code hook (approved in your Rubi panel)."

// osascriptArgs is the argv of the administrator run. The script and the prompt travel as arguments of the
// run handler, so AppleScript never has to quote them.
func osascriptArgs(p InstallPlan) []string {
	return []string{"-e", "on run argv",
		"-e", "do shell script (item 1 of argv) with administrator privileges with prompt (item 2 of argv)",
		"-e", "end run", p.FullScript(), adminPrompt}
}

func runOsascript(ctx context.Context, p InstallPlan) error {
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", osascriptArgs(p)...)
	cmd.Env = privilegedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "(-128)") || strings.Contains(string(out), "User canceled") {
			return ErrAdminCancelled
		}
		return fmt.Errorf("osascript: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// InstallHook plans, runs and verifies the installation, telling w what it does.
func InstallHook(ctx context.Context, w io.Writer, o InstallOptions) error {
	tools := defaultTools()
	if o.tools != nil {
		tools = *o.tools
	}
	p, err := planInstall(o.Src, o.Dst, o.RequireRoot, tools)
	if err != nil {
		return err
	}
	if o.WithPlugin {
		dst := o.PluginDst
		if dst == "" {
			dst = DefaultClaudePluginDir
		}
		pp, err := planClaudePlugin(claudeplugin.Files, dst, o.RequireRoot, tools)
		if err != nil {
			return &coded{CodePlugin, err}
		}
		p.Plugin = &pp
	}
	run := o.Run
	if run == nil {
		run = runOsascript
	}
	fmt.Fprintf(w, "Source:  %s\n         version %s\n         sha256 %s\n", p.Src, p.Version, p.SHA256)
	fmt.Fprintf(w, "Target:  %s\n", p.Dst)
	fmt.Fprintln(w, "That sha256 is of the file at the source path, not proof that it is a genuine release: if you got Rubi Home")
	fmt.Fprintln(w, "from a release, compare it with that release's SHA256SUMS. Root copies the file once, checks it has exactly")
	fmt.Fprintln(w, "this hash, and only then moves it into place.")
	fmt.Fprintf(w, "Script run as root (/bin/sh):\n%s", indent(p.Script))
	if p.Plugin != nil {
		p.Plugin.describe(w)
	}
	fmt.Fprintln(w, "Run via: /usr/bin/osascript, \"do shell script ... with administrator privileges\" (macOS asks for an administrator password)")
	if p.Plugin == nil {
		fmt.Fprintln(w, "Not installed: the Claude Code sister tools (a plugin for rubi_ask and the like). Add --with-claude-plugin to install them in the same step.")
	}
	if o.DryRun {
		fmt.Fprintln(w, "Dry run: nothing was run and nothing was changed.")
		return nil
	}
	fmt.Fprintln(w, "Running ...")
	if err := run(ctx, p); err != nil {
		if errors.Is(err, ErrAdminCancelled) {
			return err
		}
		return fmt.Errorf("the installation did not run: %v", err)
	}
	// Confirmation only: root already checked the hash before it moved the file into place.
	got, err := fileSHA256(p.Dst)
	if err != nil {
		return codedf(CodeVerify, "verification failed: couldn't read %s afterwards: %v", p.Dst, err)
	}
	if got != p.SHA256 {
		return codedf(CodeVerify, "verification failed: %s has sha256 %s, expected %s from %s; it was NOT updated correctly (remove it and install again)", p.Dst, got, p.SHA256, p.Src)
	}
	vt := o.VersionTimeout
	if vt <= 0 {
		vt = HookBinaryTimeout
	}
	v, err := runVersion(p.Dst, vt)
	if err != nil {
		return codedf(CodeVerify, "verification failed: %s: %v", p.Dst, err)
	}
	if v != p.Version {
		return codedf(CodeVerify, "verification failed: %s prints version %q, expected %q", p.Dst, v, p.Version)
	}
	if o.RequireRoot {
		b := InspectHookBinary(p.Dst, vt)
		if !b.Secure {
			return codedf(CodeVerify, "verification failed: %s must be owned by %s, a plain file, and not writable by %s (%s)", p.Dst, fsx.AdminName, fsx.OthersName, insecureDetail(b))
		}
	}
	fmt.Fprintf(w, "Installed %s (version %s, sha256 %s).\nVerified: the file's sha256 and `version` match the source.\n", p.Dst, v, got)
	if p.Plugin != nil {
		if err := verifyPlugin(*p.Plugin, o.RequireRoot); err != nil {
			return err
		}
		fmt.Fprintf(w, "Installed the Claude Code plugin in %s (%d files, sha256 checked).\nNext: managed settings must register it (examples/managed-settings.json in the rubi-claude-code plugin: extraKnownMarketplaces and enabledPlugins), then restart Claude Code.\n", p.Plugin.Dst, len(p.Plugin.Files))
	}
	return nil
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ") + "\n"
}

// verifyPlugin checks the installed tree: every file there with the expected sha256, and as root, owned by
// root and not writable by group or others.
func verifyPlugin(p PluginPlan, asRoot bool) error {
	for _, f := range p.Files {
		full := filepath.Join(p.Dst, filepath.FromSlash(f.Path))
		got, err := fileSHA256(full)
		if err != nil {
			return codedf(CodeVerify, "verification failed: %s: %v", full, err)
		}
		if got != f.SHA256 {
			return codedf(CodeVerify, "verification failed: %s has sha256 %s, expected %s", full, got, f.SHA256)
		}
	}
	if !asRoot {
		return nil
	}
	return filepath.WalkDir(p.Dst, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fsx.Reparse(fp, fi) {
			return codedf(CodeVerify, "verification failed: %s is a link or a junction", fp)
		}
		if ok, why := fsx.TrustedByAdmin(fp, fi); !ok {
			return codedf(CodeVerify, "verification failed: %s must be owned by %s and not writable by %s%s", fp, fsx.AdminName, fsx.OthersName, trustDetail(why))
		}
		return nil
	})
}

// trustDetail is ": <why>" where a failed trust check has a reason worth showing (Windows), else "".
func trustDetail(why string) string {
	if fsx.ExplainTrust && why != "" {
		return ": " + why
	}
	return ""
}

// insecureDetail is what the error says about a hook binary that is not trusted: on Unix its owner and mode, as it
// always did; where those mean nothing (Windows), the reason the trust check gave, or the folder above it.
func insecureDetail(b HookBinary) string {
	if !fsx.ExplainTrust {
		return fmt.Sprintf("uid %d, mode %04o", b.Owner, b.Mode.Perm())
	}
	switch {
	case b.Symlink:
		return "it is a link or a junction"
	case b.WhyFile != "":
		return "it is " + b.WhyFile
	case b.WhyInsecure != "":
		return b.WhyInsecure
	}
	return "its permissions are not an administrator's"
}
