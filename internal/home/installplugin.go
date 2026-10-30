package home

// `rubi-home install-hook --with-claude-plugin`: installs the Claude Code marketplace embedded in this
// binary (claude-plugin/, marketplace "rubi" with the plugin rubi-sisters) to a root-owned directory, in the
// same privileged run as the hook binary. Nothing user-writable is read by root: the file contents travel
// inside the script (one base64 literal per file), root decodes them into a fresh directory next to the
// target, checks every file's sha256, makes the tree root-owned, and swaps it in with a rename. The old
// directory is moved aside first and removed only after the rename worked (put back when it failed).

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	claudeplugin "github.com/Deikus-LXXVII/rubi/claude-plugin"
)

// DefaultClaudePluginDir is where the marketplace is installed (and what managed settings point at).
const DefaultClaudePluginDir = "/Library/Application Support/Rubi Home/claude-plugin"

// maxPluginScript bounds the privileged script (it travels as one argument).
const maxPluginScript = 256 << 10

// PluginFile is one file of the embedded marketplace.
type PluginFile struct {
	Path   string // relative, slash-separated
	Data   []byte
	SHA256 string
}

// PluginPlan is what the plugin part of an install is about to do.
type PluginPlan struct {
	Dst    string
	Files  []PluginFile
	Script string
}

var pluginPathRE = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)

// EmbeddedPluginFiles lists the embedded marketplace's files, sorted.
func EmbeddedPluginFiles() ([]PluginFile, error) { return pluginFiles(claudeplugin.Files) }

func pluginFiles(fsys fs.FS) ([]PluginFile, error) {
	var out []PluginFile
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s isn't a regular file", p)
		}
		if p == "embed.go" {
			return nil
		}
		if !pluginPathRE.MatchString(p) || strings.Contains(p, "..") || path.Clean(p) != p {
			return fmt.Errorf("the plugin file name %q can't be quoted safely", p)
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out = append(out, PluginFile{Path: p, Data: b, SHA256: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("this binary has no embedded Claude Code plugin")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// pluginScript is what root runs for the plugin. With asRoot it demands that the directory's ancestors are
// root-owned and not group/world-writable, and chowns the tree before the swap.
func pluginScript(t installTools, dst string, files []PluginFile, asRoot bool) string {
	dir := filepath.Dir(dst)
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	w("set -e; umask 022")
	w("pdst='%s'; pdir='%s'; pparent='%s'", dst, dir, filepath.Dir(dir))
	if asRoot {
		w("pchk() { d=$1; while :; do")
		w("  s=$(%s \"$d\") || exit 1", t.Stat)
		w("  [ \"${s%%%%:*}\" = 0 ] || { echo \"$d is not owned by root\" >&2; exit 1; }")
		w("  [ $((0${s#*:} & 022)) -eq 0 ] || { echo \"$d is writable by group or others\" >&2; exit 1; }")
		w("  [ \"$d\" = / ] && break")
		w("  d=$(%s \"$d\")", t.Dirname)
		w("done; }")
		w("pchk \"$pparent\"")
	}
	w("[ -d \"$pdir\" ] || %s \"$pdir\"", t.Mkdir)
	w("[ ! -L \"$pdir\" ] || { echo \"$pdir is a symlink\" >&2; exit 1; }")
	if asRoot {
		w("pchk \"$pdir\"")
	}
	w("[ ! -e \"$pdst\" ] || { [ -d \"$pdst\" ] && [ ! -L \"$pdst\" ]; } || { echo \"$pdst is not a plain directory\" >&2; exit 1; }")
	// Leftovers of an earlier run that was cut off (temporary trees, a moved-aside old tree).
	w("for x in \"$pdir\"/.claude-plugin.*; do [ -e \"$x\" ] || [ -L \"$x\" ] || continue; %s -rf \"$x\"; done", t.Rm)
	w("ptmp=$(%s -d \"$pdir/.claude-plugin.XXXXXX\")", t.Mktemp)
	w("pold=")
	w("trap '%s -rf \"$ptmp\"' EXIT", t.Rm)
	dirs := map[string]bool{}
	for _, f := range files {
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	var dl []string
	for d := range dirs {
		dl = append(dl, d)
	}
	sort.Strings(dl) // parents sort before their children
	for _, d := range dl {
		w("%s \"$ptmp/%s\"", t.Mkdir, d)
	}
	for _, f := range files {
		w("printf %%s '%s' | %s > \"$ptmp/%s\"", base64.StdEncoding.EncodeToString(f.Data), t.B64D, f.Path)
		w("[ \"$(%s < \"$ptmp/%s\" | %s -d' ' -f1)\" = '%s' ] || { echo \"%s was not decoded intact; nothing installed\" >&2; exit 1; }",
			t.Sha, f.Path, t.Cut, f.SHA256, f.Path)
	}
	if asRoot {
		w("%s -R root:%s \"$ptmp\"", t.Chown, t.Group)
	}
	w("%s -R go-w \"$ptmp\"", t.Chmod)
	w("%s 0755 \"$ptmp\"", t.Chmod)
	// The old tree is kept until the new one is in place.
	w("if [ -e \"$pdst\" ]; then pold=\"$pdir/.claude-plugin.old.$$\"; %s \"$pdst\" \"$pold\"; fi", t.Mv)
	w("if %s \"$ptmp\" \"$pdst\"; then", t.Mv)
	w("  [ -z \"$pold\" ] || %s -rf \"$pold\"", t.Rm)
	w("else")
	w("  [ -z \"$pold\" ] || %s \"$pold\" \"$pdst\"", t.Mv)
	w("  echo \"couldn't move the new plugin into place; the old one is kept\" >&2; exit 1")
	w("fi")
	return b.String()
}

// PlanClaudePlugin checks the target and builds the plugin part of the plan.
func PlanClaudePlugin(dst string, requireRoot bool) (PluginPlan, error) {
	return planClaudePlugin(claudeplugin.Files, dst, requireRoot, defaultTools())
}

func planClaudePlugin(fsys fs.FS, dst string, requireRoot bool, tools installTools) (PluginPlan, error) {
	if why := unsafePath(dst); why != "" {
		return PluginPlan{}, fmt.Errorf("the plugin target path %q is refused: %s", dst, why)
	}
	files, err := pluginFiles(fsys)
	if err != nil {
		return PluginPlan{}, err
	}
	if li, err := os.Lstat(dst); err == nil && (!li.IsDir() || li.Mode()&fs.ModeSymlink != 0) {
		return PluginPlan{}, fmt.Errorf("%s exists and isn't a plain directory; remove it by hand and run install-hook again", dst)
	}
	if requireRoot {
		probe := dst
		for { // the nearest directory that exists decides (the script creates the one directory below it)
			if _, err := os.Stat(filepath.Dir(probe)); err == nil {
				break
			}
			if probe = filepath.Dir(probe); probe == "/" || probe == "." {
				break
			}
		}
		if why := insecureAncestors(probe); why != "" {
			return PluginPlan{}, fmt.Errorf("refusing to install the Claude Code plugin into %s: %s. Its files are only safe where no one but root can replace them", filepath.Dir(dst), why)
		}
	}
	sc := pluginScript(tools, dst, files, requireRoot)
	if len(sc) > maxPluginScript {
		return PluginPlan{}, fmt.Errorf("the plugin is too large to install in one step (%d bytes)", len(sc))
	}
	return PluginPlan{Dst: dst, Files: files, Script: sc}, nil
}

// describe tells w what the plugin step installs; the base64 of each file is left out of the printed script
// (the sha256 of every file is listed, and root checks it).
func (p PluginPlan) describe(w io.Writer) {
	fmt.Fprintf(w, "Claude Code plugin (marketplace \"rubi\", plugin rubi-sisters) to: %s\n", p.Dst)
	for _, f := range p.Files {
		fmt.Fprintf(w, "  %-52s %6d bytes  sha256 %s\n", f.Path, len(f.Data), f.SHA256)
	}
	short := regexp.MustCompile(`printf %s '[A-Za-z0-9+/=]{40,}'`).ReplaceAllStringFunc(p.Script, func(s string) string {
		return fmt.Sprintf("printf %%s '<%d characters of base64>'", len(s)-len("printf %s ''"))
	})
	fmt.Fprintf(w, "Plugin script run as root (/bin/sh; the base64 of each file is shortened here, root decodes it and checks the sha256 above):\n%s", indent(short))
}
