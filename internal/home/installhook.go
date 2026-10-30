package home

// `rubi-home install-hook`: copies this binary to the root-owned hook path with administrator rights.
// The privileged step runs through a seam (InstallRunner), so tests copy to a temporary path instead.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Deikus-LXXVII/rubi/internal/version"
)

// InstallPlan is what install-hook is about to do.
type InstallPlan struct {
	Src, Dst string
	SHA256   string // of Src
	Version  string
	Group    string // wheel on macOS
	Script   string // the shell script root runs (see installScript)
	Method   string // "osascript" or "sudo"
}

// InstallRunner runs the plan's privileged script.
type InstallRunner func(ctx context.Context, p InstallPlan) error

// InstallOptions are install-hook's inputs.
type InstallOptions struct {
	Src, Dst string
	DryRun   bool
	Sudo     bool // use sudo (in a terminal) instead of the macOS password dialog
	// Run is the privileged runner; nil uses osascript, or sudo with Sudo.
	Run InstallRunner
	// RequireRoot demands root-owned, not group/world-writable ancestors of the target and a root-owned
	// result, and puts the same checks and the chown into the script. Tests that install to a temporary
	// path without root turn it off.
	RequireRoot bool
	// VersionTimeout bounds `dst version` (HookBinaryTimeout when zero).
	VersionTimeout time.Duration
}

// privilegedPath is the only PATH the privileged commands get (and every tool in the script is absolute).
const privilegedPath = "PATH=/usr/bin:/bin:/usr/sbin:/sbin"

// installTools are the absolute paths of the tools the script uses.
type installTools struct {
	Stat, Cat, Sha, Cut, Dirname, Mktemp, Rm, Chown, Chmod, Mv string
	StatFmt                                                    string // stat's format option and string
	Group                                                      string
}

func defaultTools() installTools {
	if runtime.GOOS == "darwin" {
		return installTools{Stat: "/usr/bin/stat -L -f '%u:%Lp'", Cat: "/bin/cat", Sha: "/usr/bin/shasum -a 256",
			Cut: "/usr/bin/cut", Dirname: "/usr/bin/dirname", Mktemp: "/usr/bin/mktemp", Rm: "/bin/rm",
			Chown: "/usr/sbin/chown", Chmod: "/bin/chmod", Mv: "/bin/mv", Group: "wheel"}
	}
	return installTools{Stat: "/usr/bin/stat -L -c '%u:%a'", Cat: "/bin/cat", Sha: "/usr/bin/sha256sum",
		Cut: "/usr/bin/cut", Dirname: "/usr/bin/dirname", Mktemp: "/usr/bin/mktemp", Rm: "/bin/rm",
		Chown: "/bin/chown", Chmod: "/bin/chmod", Mv: "/bin/mv", Group: "root"}
}

// installScript is what root runs. The source is read once, as root, into a temporary file in the target's
// directory; root hashes that copy and goes on only when it equals want (the hash the user was shown), then
// makes it root-owned, mode 0755, and renames it over the target (atomic). Nothing the user can write to is
// read twice or used after the check. The temporary file is removed on any failure. With asRoot the script
// also demands that the directory and all its ancestors are root-owned and not group/world-writable. Every
// path in it is single-quoted; unsafePath keeps quotes out of them.
func installScript(t installTools, src, dst, want string, asRoot bool) string {
	dir := filepath.Dir(dst)
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	w("set -e; umask 022")
	w("src='%s'; dst='%s'; dir='%s'; want='%s'", src, dst, dir, want)
	if asRoot {
		w("d=$dir")
		w("while :; do")
		w("  s=$(%s \"$d\") || exit 1", t.Stat)
		w("  [ \"${s%%%%:*}\" = 0 ] || { echo \"$d is not owned by root\" >&2; exit 1; }")
		w("  [ $((0${s#*:} & 022)) -eq 0 ] || { echo \"$d is writable by group or others\" >&2; exit 1; }")
		w("  [ \"$d\" = / ] && break")
		w("  d=$(%s \"$d\")", t.Dirname)
		w("done")
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

func planInstall(src, dst string, requireRoot bool, tools installTools) (InstallPlan, error) {
	for _, x := range []struct{ what, p string }{{"the source", src}, {"the target", dst}} {
		if why := unsafePath(x.p); why != "" {
			return InstallPlan{}, fmt.Errorf("%s path %q is refused: %s", x.what, x.p, why)
		}
	}
	fi, err := os.Stat(src)
	if err != nil || !fi.Mode().IsRegular() {
		return InstallPlan{}, fmt.Errorf("the source %s isn't a readable file", src)
	}
	dir := filepath.Dir(dst)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return InstallPlan{}, fmt.Errorf("the directory %s doesn't exist; create it first, owned by root and writable by no one else (sudo mkdir -p %s), then run install-hook again", dir, dir)
	}
	if li, err := os.Lstat(dst); err == nil && !li.Mode().IsRegular() {
		return InstallPlan{}, fmt.Errorf("%s exists and isn't a plain file (a symlink or something else); remove it by hand and run install-hook again", dst)
	}
	if requireRoot {
		if why := insecureAncestors(dst); why != "" {
			return InstallPlan{}, fmt.Errorf("refusing to install into %s: %s. A hook binary is only safe where no one but root can replace it or its directories", dir, why)
		}
	}
	if rs, e1 := filepath.EvalSymlinks(src); e1 == nil {
		if rd, e2 := filepath.EvalSymlinks(dst); e2 == nil && rs == rd {
			return InstallPlan{}, fmt.Errorf("%s is this binary itself; run install-hook from the helper copy (%s)", dst, "~/.rubi-home/bin/rubi-home")
		}
	}
	sum, err := fileSHA256(src)
	if err != nil {
		return InstallPlan{}, fmt.Errorf("couldn't read %s: %v", src, err)
	}
	return InstallPlan{Src: src, Dst: dst, SHA256: sum, Version: version.Version, Group: tools.Group,
		Script: installScript(tools, src, dst, sum, requireRoot)}, nil
}

// privilegedEnv is the scrubbed environment for osascript and sudo.
func privilegedEnv() []string { return []string{privilegedPath} }

func runOsascript(ctx context.Context, p InstallPlan) error {
	// The script travels as an argument of the run handler, so AppleScript never has to quote it.
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript",
		"-e", "on run argv", "-e", "do shell script (item 1 of argv) with administrator privileges", "-e", "end run", p.Script)
	cmd.Env = privilegedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runSudo(ctx context.Context, p InstallPlan) error {
	cmd := exec.CommandContext(ctx, "/usr/bin/sudo", "/bin/sh", "-c", p.Script)
	cmd.Env = privilegedEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// InstallHook plans, runs and verifies the installation, telling w what it does.
func InstallHook(ctx context.Context, w io.Writer, o InstallOptions) error {
	p, err := PlanInstallHook(o.Src, o.Dst, o.RequireRoot)
	if err != nil {
		return err
	}
	method := "osascript"
	run := o.Run
	if o.Sudo {
		method = "sudo"
	}
	p.Method = method
	if run == nil {
		run = runOsascript
		if o.Sudo {
			run = runSudo
		}
	}
	fmt.Fprintf(w, "Source:  %s\n         version %s\n         sha256 %s\n", p.Src, p.Version, p.SHA256)
	fmt.Fprintf(w, "Target:  %s\n", p.Dst)
	fmt.Fprintln(w, "That sha256 is of the file at the source path, not proof that it is a genuine release: if you got Rubi Home")
	fmt.Fprintln(w, "from a release, compare it with that release's SHA256SUMS. Root copies the file once, checks it has exactly")
	fmt.Fprintln(w, "this hash, and only then moves it into place.")
	fmt.Fprintf(w, "Script run as root (/bin/sh):\n%s", indent(p.Script))
	if method == "sudo" {
		fmt.Fprintln(w, "Run via: /usr/bin/sudo (asks for your password in this terminal)")
	} else {
		fmt.Fprintln(w, "Run via: /usr/bin/osascript, \"do shell script ... with administrator privileges\" (macOS asks for an administrator password)")
	}
	if o.DryRun {
		fmt.Fprintln(w, "Dry run: nothing was run and nothing was changed.")
		return nil
	}
	fmt.Fprintln(w, "Running ...")
	if err := run(ctx, p); err != nil {
		return fmt.Errorf("the installation did not run: %v", err)
	}
	// Confirmation only: root already checked the hash before it moved the file into place.
	got, err := fileSHA256(p.Dst)
	if err != nil {
		return fmt.Errorf("verification failed: couldn't read %s afterwards: %v", p.Dst, err)
	}
	if got != p.SHA256 {
		return fmt.Errorf("verification failed: %s has sha256 %s, expected %s from %s; it was NOT updated correctly (remove it or run install-hook again)", p.Dst, got, p.SHA256, p.Src)
	}
	vt := o.VersionTimeout
	if vt <= 0 {
		vt = HookBinaryTimeout
	}
	v, err := runVersion(p.Dst, vt)
	if err != nil {
		return fmt.Errorf("verification failed: %s: %v", p.Dst, err)
	}
	if v != p.Version {
		return fmt.Errorf("verification failed: %s prints version %q, expected %q", p.Dst, v, p.Version)
	}
	if o.RequireRoot {
		b := InspectHookBinary(p.Dst, vt)
		if !b.Secure {
			return fmt.Errorf("verification failed: %s must be owned by root, a plain file, and not writable by group or others (uid %d, mode %04o)", p.Dst, b.Owner, b.Mode.Perm())
		}
	}
	fmt.Fprintf(w, "Installed %s (version %s, sha256 %s).\nVerified: the file's sha256 and `version` match the source.\n", p.Dst, v, got)
	return nil
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ") + "\n"
}
