package home

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

func pluginHostTools(t *testing.T) installTools {
	t.Helper()
	tl := hostTools(t)
	look := func(n string) string {
		p, err := exec.LookPath(n)
		if err != nil {
			t.Skipf("no %s on this host", n)
		}
		return p
	}
	tl.Mkdir = look("mkdir")
	tl.B64D = look("base64") + " -d"
	if runtime.GOOS == "darwin" {
		tl.B64D = look("base64") + " -D"
	}
	return tl
}

func TestEmbeddedPluginFiles(t *testing.T) {
	files, err := EmbeddedPluginFiles()
	if err != nil {
		t.Fatal(err)
	}
	by := map[string][]byte{}
	for _, f := range files {
		by[f.Path] = f.Data
		if len(f.SHA256) != 64 {
			t.Errorf("%s: sha256 %q", f.Path, f.SHA256)
		}
	}
	for _, want := range []string{".claude-plugin/marketplace.json", "rubi-sisters/.claude-plugin/plugin.json", "rubi-sisters/.mcp.json",
		"rubi-sisters/skills/rubi-sisters/SKILL.md", "rubi-sisters/skills/rubi-claude-pair/SKILL.md"} {
		if len(by[want]) == 0 {
			t.Errorf("%s isn't embedded", want)
		}
	}
	if _, has := by["embed.go"]; has || len(files) != 5 {
		t.Errorf("files: %d, embed.go %v", len(files), has)
	}
	var mk struct {
		Name    string
		Plugins []struct{ Name, Source string }
	}
	if err := json.Unmarshal(by[".claude-plugin/marketplace.json"], &mk); err != nil || mk.Name != "rubi" ||
		len(mk.Plugins) != 1 || mk.Plugins[0].Name != "rubi-sisters" || mk.Plugins[0].Source != "./rubi-sisters" {
		t.Errorf("marketplace: %+v %v", mk, err)
	}
	var mcp struct {
		McpServers map[string]struct {
			Command string
			Args    []string
		}
	}
	if err := json.Unmarshal(by["rubi-sisters/.mcp.json"], &mcp); err != nil || len(mcp.McpServers) != 1 {
		t.Fatalf(".mcp.json: %v", err)
	}
	for _, s := range mcp.McpServers {
		if s.Command != DefaultHookBinary || len(s.Args) != 1 || s.Args[0] != "claude-mcp" {
			t.Errorf("mcp server: %+v (the root-owned binary, never a user-writable one)", s)
		}
	}
	if !strings.Contains(string(by["rubi-sisters/skills/rubi-sisters/SKILL.md"]), "rubi_ask") ||
		!strings.Contains(string(by["rubi-sisters/skills/rubi-sisters/SKILL.md"]), "```rubi-ask") {
		t.Error("the skill prefers the tools and keeps the block as a fallback")
	}
	if strings.Contains(string(by["rubi-sisters/.claude-plugin/plugin.json"]), "hooks") {
		t.Error("the plugin must carry no hooks")
	}
}

func TestPluginScriptShape(t *testing.T) {
	files, _ := EmbeddedPluginFiles()
	for _, tools := range []installTools{defaultTools(), {Stat: "/usr/bin/stat -L -c '%u:%a'", Cat: "/bin/cat", Sha: "/usr/bin/sha256sum", Cut: "/usr/bin/cut",
		Dirname: "/usr/bin/dirname", Mktemp: "/usr/bin/mktemp", Rm: "/bin/rm", Chown: "/bin/chown", Chmod: "/bin/chmod", Mv: "/bin/mv", Group: "root",
		Mkdir: "/bin/mkdir", B64D: "/usr/bin/base64 -d"}} {
		sc := pluginScript(tools, DefaultClaudePluginDir, files, true)
		pos := func(sub string) int { return strings.Index(sc, sub) }
		order := []string{"pchk \"$pparent\"", "mkdir", "pchk \"$pdir\"", "mktemp", "trap ", "printf %s", "= '", "chown", "go-w", "0755 \"$ptmp\"", "\"$pdst\" \"$pold\"", "\"$ptmp\" \"$pdst\""}
		last := -1
		for _, o := range order {
			i := strings.Index(sc[max(last, 0):], o)
			if i < 0 {
				t.Fatalf("%q missing after %d:\n%s", o, last, sc)
			}
			last = max(last, 0) + i
		}
		_ = pos
		for _, want := range []string{"set -e", "not owned by root", "writable by group or others", "pdst='" + DefaultClaudePluginDir + "'",
			"was not decoded intact; nothing installed", "pold=\"$pdir/.claude-plugin.old.$$\""} {
			if !strings.Contains(sc, want) {
				t.Errorf("script lacks %q", want)
			}
		}
		// Nothing user-writable is read: the script reads no path except its own temporary tree and the target.
		for _, bad := range []string{"$src", "/Users/", "/home/", " cat "} {
			if strings.Contains(sc, bad) {
				t.Errorf("script mentions %q", bad)
			}
		}
		for _, l := range strings.Split(sc, "\n") { // every command is an absolute path
			for _, tool := range []string{"base64", "mkdir", "mktemp", "chmod", "chown", "shasum", "sha256sum", "mv ", "rm "} {
				if i := strings.Index(l, tool); i > 0 && !strings.Contains(l[:i], "/") && !strings.Contains(l, "echo") && !strings.HasPrefix(strings.TrimSpace(l), "pdst=") &&
					!strings.Contains(l, "printf %s '") {
					t.Errorf("relative tool %q in %q", tool, l)
				}
			}
		}
		for _, asRoot := range []bool{true, false} { // the root-only parts must at least parse
			if out, err := exec.Command("/bin/sh", "-n", "-c", pluginScript(tools, DefaultClaudePluginDir, files, asRoot)).CombinedOutput(); err != nil {
				t.Errorf("asRoot=%v does not parse: %v %s", asRoot, err, out)
			}
		}
		if strings.Contains(pluginScript(tools, "/a/b/c", files, false), "chown") || strings.Contains(pluginScript(tools, "/a/b/c", files, false), "pchk") {
			t.Error("the non-root script chowns or checks ancestors")
		}
	}
}

func pluginDirContents(t *testing.T, dst string) map[string]string {
	t.Helper()
	got := map[string]string{}
	_ = filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dst, p)
			got[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return got
}

func TestPluginScriptInstallsAndReplaces(t *testing.T) {
	tools := pluginHostTools(t)
	files, _ := EmbeddedPluginFiles()
	base := t.TempDir()
	dst := filepath.Join(base, "Rubi Home", "claude-plugin") // the parent doesn't exist yet; a space in the path
	sc := pluginScript(tools, dst, files, false)
	if out, err := runScript(t, sc); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got := pluginDirContents(t, dst)
	if len(got) != len(files) {
		t.Fatalf("%d files installed, want %d", len(got), len(files))
	}
	for _, f := range files {
		if got[f.Path] != string(f.Data) {
			t.Errorf("%s differs", f.Path)
		}
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o755 {
		t.Errorf("directory mode %v", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Join(dst, "rubi-sisters", ".mcp.json")); fi.Mode().Perm()&0o022 != 0 {
		t.Errorf("file mode %v", fi.Mode())
	}
	left := func() []string {
		m, _ := filepath.Glob(filepath.Join(filepath.Dir(dst), ".claude-plugin.*"))
		return m
	}
	if len(left()) != 0 {
		t.Fatalf("temporary entries left: %v", left())
	}

	// A second install replaces the directory whole: a file of the old tree that is gone from the new one is gone.
	if err := os.WriteFile(filepath.Join(dst, "stale.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runScript(t, sc); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale.txt")); err == nil {
		t.Fatal("the old tree's file survived")
	}
	if len(pluginDirContents(t, dst)) != len(files) || len(left()) != 0 {
		t.Fatalf("after the second install: %v %v", pluginDirContents(t, dst), left())
	}
}

// If the rename fails the old directory is still there, whole, and nothing else is left.
func TestPluginScriptFailureKeepsTheOldDirectory(t *testing.T) {
	tools := pluginHostTools(t)
	files, _ := EmbeddedPluginFiles()
	base := t.TempDir()
	dst := filepath.Join(base, "claude-plugin")
	if out, err := runScript(t, pluginScript(tools, dst, files, false)); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dst, "mine.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	real := tools.Mv
	fake := filepath.Join(t.TempDir(), "mv")
	// Moving the old tree aside and back works; moving the new tree into place fails.
	body := "#!/bin/sh\ncase \"$1\" in *.claude-plugin.old.*) exec " + real + " \"$@\";; esac\ncase \"$2\" in */claude-plugin) echo boom >&2; exit 1;; esac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(fake, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	tools.Mv = fake
	// Another version of the plugin is the one being installed.
	newFiles := append([]PluginFile(nil), files...)
	newFiles[0].Data = []byte(`{"changed":true}`)
	newFiles[0].SHA256 = sha256Hex(newFiles[0].Data)
	out, err := runScript(t, pluginScript(tools, dst, newFiles, false))
	if err == nil || !strings.Contains(out, "the old one is kept") {
		t.Fatalf("expected a failure: %v %s", err, out)
	}
	got := pluginDirContents(t, dst)
	if got["mine.txt"] != "kept" || len(got) != len(files)+1 {
		t.Fatalf("the old directory changed: %v", got)
	}
	for _, f := range files {
		if got[f.Path] != string(f.Data) {
			t.Errorf("%s changed", f.Path)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(base, ".claude-plugin.*")); len(m) != 0 {
		t.Fatalf("temporary entries left: %v", m)
	}
}

func TestPluginScriptRefusesACorruptFile(t *testing.T) {
	tools := pluginHostTools(t)
	files, _ := EmbeddedPluginFiles()
	base := t.TempDir()
	dst := filepath.Join(base, "claude-plugin")
	bad := append([]PluginFile(nil), files...)
	bad[1].SHA256 = strings.Repeat("0", 64)
	out, err := runScript(t, pluginScript(tools, dst, bad, false))
	if err == nil || !strings.Contains(out, "nothing installed") {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("installed a file that failed its check")
	}
	if m, _ := filepath.Glob(filepath.Join(base, ".claude-plugin.*")); len(m) != 0 {
		t.Fatalf("temporary entries left: %v", m)
	}
	// A target that is a plain file or a symlink is refused by the script as well.
	f := filepath.Join(base, "afile")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	if out, err := runScript(t, pluginScript(tools, f, files, false)); err == nil || !strings.Contains(out, "not a plain directory") {
		t.Fatalf("%v %s", err, out)
	}
}

func TestInstallHookWithPluginDryRun(t *testing.T) {
	src, dst := installFixture(t)
	pdst := filepath.Join(filepath.Dir(dst), "plugin-dir")
	called := false
	var out bytes.Buffer
	err := InstallHook(context.Background(), &out, InstallOptions{Src: src, Dst: dst, DryRun: true, WithPlugin: true, PluginDst: pdst,
		Run: func(context.Context, InstallPlan) error { called = true; return nil }})
	if err != nil || called {
		t.Fatalf("err %v, runner called %v", err, called)
	}
	files, _ := EmbeddedPluginFiles()
	for _, want := range []string{"Claude Code plugin", pdst, "Plugin script run as root", "characters of base64", "Dry run: nothing was run", "sha256 " + files[0].SHA256,
		"rubi-sisters/.mcp.json", "was not decoded intact"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry run lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "Add --with-claude-plugin") {
		t.Error("the hint is for runs without the flag")
	}
	if _, err := os.Stat(pdst); err == nil {
		t.Fatal("dry run wrote the plugin")
	}
	// Without the flag the plan says what is left out.
	out.Reset()
	if err := InstallHook(context.Background(), &out, InstallOptions{Src: src, Dst: dst, DryRun: true}); err != nil ||
		!strings.Contains(out.String(), "--with-claude-plugin") || strings.Contains(out.String(), "Plugin script") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

func TestInstallHookWithPluginRuns(t *testing.T) {
	tools := pluginHostTools(t)
	src, dst := installFixture(t)
	pdst := filepath.Join(filepath.Dir(dst), "claude-plugin")
	var ran InstallPlan
	var out bytes.Buffer
	run := func(ctx context.Context, p InstallPlan) error {
		ran = p
		if err := copyRunner(nil)(ctx, p); err != nil {
			return err
		}
		o, err := exec.Command("/bin/sh", "-c", p.Plugin.Script).CombinedOutput()
		if err != nil {
			t.Logf("%s", o)
		}
		return err
	}
	err := InstallHook(context.Background(), &out, InstallOptions{Src: src, Dst: dst, WithPlugin: true, PluginDst: pdst, Run: run, tools: &tools})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	files, _ := EmbeddedPluginFiles()
	if got := pluginDirContents(t, pdst); len(got) != len(files) {
		t.Fatalf("installed: %v", got)
	}
	if !strings.Contains(out.String(), "Installed the Claude Code plugin") || !strings.Contains(out.String(), "extraKnownMarketplaces") {
		t.Fatalf("output:\n%s", out.String())
	}
	// One privileged run: the binary's script and then the plugin's, in a subshell with its own trap.
	full := ran.FullScript()
	if !strings.HasPrefix(full, ran.Script) || !strings.Contains(full, "\n(\n"+ran.Plugin.Script) || !strings.HasSuffix(full, ")\n") {
		t.Fatalf("full script:\n%s", full)
	}
	if (InstallPlan{Script: "x"}).FullScript() != "x" {
		t.Fatal("without a plugin the script is the binary's alone")
	}
	// The combined script runs for real (with the binary step stubbed out by an empty one).
	if o, err := runScript(t, "set -e\n(\n"+ran.Plugin.Script+")\n"); err != nil {
		t.Fatalf("%v %s", err, o)
	}

	// A plugin that didn't land is a failure with a clear message.
	os.RemoveAll(pdst)
	err = InstallHook(context.Background(), &bytes.Buffer{}, InstallOptions{Src: src, Dst: dst, WithPlugin: true, PluginDst: pdst, Run: copyRunner(nil), tools: &tools})
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("%v", err)
	}
	// A tampered file is caught by the verification.
	err = InstallHook(context.Background(), &bytes.Buffer{}, InstallOptions{Src: src, Dst: dst, WithPlugin: true, PluginDst: pdst, tools: &tools,
		Run: func(ctx context.Context, p InstallPlan) error {
			_ = copyRunner(nil)(ctx, p)
			if o, err := exec.Command("/bin/sh", "-c", p.Plugin.Script).CombinedOutput(); err != nil {
				t.Logf("%s", o)
				return err
			}
			return os.WriteFile(filepath.Join(pdst, "rubi-sisters", ".mcp.json"), []byte("{}"), 0o644)
		}})
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("%v", err)
	}
}

func TestPlanClaudePluginRefusals(t *testing.T) {
	for _, dst := range []string{"relative/claude-plugin", "/tmp/it's/claude-plugin", "/tmp/a$b/claude-plugin"} {
		if _, err := PlanClaudePlugin(dst, false); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%q: %v", dst, err)
		}
	}
	base := t.TempDir()
	f := filepath.Join(base, "file")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	if _, err := PlanClaudePlugin(f, false); err == nil || !strings.Contains(err.Error(), "plain directory") {
		t.Errorf("a file: %v", err)
	}
	link := filepath.Join(base, "link")
	_ = os.Symlink(base, link)
	if _, err := PlanClaudePlugin(link, false); err == nil || !strings.Contains(err.Error(), "plain directory") {
		t.Errorf("a symlink: %v", err)
	}
	if os.Getuid() != 0 {
		if _, err := PlanClaudePlugin(filepath.Join(base, "x", "claude-plugin"), true); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("a user-owned parent: %v", err)
		}
	}
	// A name that can't be quoted safely is refused (a test file system stands in for the embed).
	_, err := planClaudePlugin(fstest.MapFS{"a b.txt": {Data: []byte("x")}}, filepath.Join(base, "p"), false, defaultTools())
	if err == nil || !strings.Contains(err.Error(), "quoted safely") {
		t.Errorf("odd name: %v", err)
	}
	_, err = planClaudePlugin(fstest.MapFS{"a.txt": {Data: bytes.Repeat([]byte("x"), 400<<10)}}, filepath.Join(base, "p"), false, defaultTools())
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("large: %v", err)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Leftovers of a cut-off earlier run are swept before a new install, and only those.
func TestPluginScriptSweepsStaleTemporaryDirectories(t *testing.T) {
	tools := pluginHostTools(t)
	files, _ := EmbeddedPluginFiles()
	base := t.TempDir()
	dst := filepath.Join(base, "claude-plugin")
	for _, d := range []string{".claude-plugin.AbCdEf", ".claude-plugin.old.1234"} {
		_ = os.MkdirAll(filepath.Join(base, d, "x"), 0o755)
	}
	_ = os.WriteFile(filepath.Join(base, "keep.txt"), []byte("k"), 0o644)
	if out, err := runScript(t, pluginScript(tools, dst, files, false)); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if m, _ := filepath.Glob(filepath.Join(base, ".claude-plugin.*")); len(m) != 0 {
		t.Fatalf("stale entries left: %v", m)
	}
	if _, err := os.Stat(filepath.Join(base, "keep.txt")); err != nil {
		t.Fatal("swept something else")
	}
	if len(pluginDirContents(t, dst)) != len(files) {
		t.Fatal("not installed")
	}
}
