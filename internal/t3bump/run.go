package t3bump

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Runner runs a command; the tool's only way to start a process (tests give it a fake).
type Runner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)
}

// ExecRunner is the real one.
type ExecRunner struct{}

// Run runs the command with the given extra environment and returns its combined output.
func (ExecRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.Bytes(), err
}

// Options configure Run.
type Options struct {
	Root   string // the repository root
	Client *Client
	Runner Runner
	// Date is today as YYYY-MM-DD (the "checked" field).
	Date string
	// SkipContract leaves out the install and the contract tests.
	SkipContract bool
	// UsedTools are the tools whose schemas are compared (the client's UsedTools).
	UsedTools []string
	// TempDir makes the folder the install goes to (os.MkdirTemp when nil).
	TempDir func() (string, error)
}

// Result is what Run found.
type Result struct {
	Old, New  Upstream
	Changed   bool
	Pin       Pin
	Contract  string // "passed", "failed" or "skipped"
	Install   string // "passed", "failed" or "skipped"
	Changes   []Change
	Log       map[string]string // the tail of each command's output, by name
	ToolsNote string
}

func tail(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = "..." + s[len(s)-n:]
	}
	return s
}

// Run finds the newest nightly, writes the pin, and (unless skipped) installs it into a temporary folder, runs the
// contract tests against it and compares the tools' input schemas with the snapshot. It does not touch git.
func Run(ctx context.Context, o Options) (Result, error) {
	res := Result{Log: map[string]string{}, Contract: "skipped", Install: "skipped"}
	old, err := Read(o.Root)
	if err != nil {
		return res, fmt.Errorf("t3/upstream.json: %w", err)
	}
	res.Old = old
	rel, err := o.Client.Newest(ctx)
	if err != nil {
		return res, err
	}
	if rel.Version == old.Version {
		res.New = old
		return res, nil
	}
	if cur, ok := ParseTag("v" + old.Version); ok && rel.Less(cur) {
		return res, fmt.Errorf("the newest nightly (%s) is older than the pin (%s); not moving back", rel.Tag, old.Version)
	}
	pin, err := o.Client.Fetch(ctx, rel)
	if err != nil {
		return res, err
	}
	res.Pin = pin
	res.New = Apply(old, pin, o.Date)
	res.Changed = true
	if err := Write(o.Root, res.New); err != nil {
		return res, err
	}
	if o.SkipContract {
		return res, nil
	}
	if o.Runner == nil {
		o.Runner = ExecRunner{}
	}
	tmp := o.TempDir
	if tmp == nil {
		tmp = func() (string, error) { return os.MkdirTemp("", "t3-bump-") }
	}
	dir, err := tmp()
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(dir)

	// 1. install the pin the way Rubi Home does (hash-checked), into the folder
	out, err := o.Runner.Run(ctx, o.Root, []string{"RUBI_T3_INSTALL_LIVE=1", "RUBI_T3_INSTALL_DIR=" + dir},
		"go", "test", "./internal/home", "-run", "^TestLiveT3Install$", "-count=1", "-v", "-timeout", "30m")
	res.Log["install"] = tail(out, 3000)
	if err != nil {
		res.Install = "failed"
		res.Contract = "not run: the pin did not install"
		return res, nil
	}
	res.Install = "passed"
	node := filepath.Join(dir, "t3", "runtime", "node", "bin", "node")
	bin := filepath.Join(dir, "t3", "runtime", "pkg", "bin", "t3.mjs")
	rt := filepath.Join(dir, "t3", "runtime", "t3home")
	for _, f := range []string{node, bin} {
		if _, err := os.Stat(f); err != nil {
			res.Install = "failed"
			res.Contract = "not run: the install left no " + filepath.Base(f)
			return res, nil
		}
	}
	env := []string{"RUBI_T3_CONTRACT=1", "RUBI_T3_BIN=" + bin, "RUBI_T3_NODE=" + node, "RUBI_T3_RUNTIME_HOME=" + rt}

	// 2. the schemas of the tools Rubi uses, as the new T3 gives them, against the snapshot
	toolsOut := filepath.Join(dir, "tools-new.json")
	out, err = o.Runner.Run(ctx, o.Root, append(append([]string(nil), env...), "RUBI_T3_TOOLS_OUT="+toolsOut),
		"go", "test", "./internal/t3", "-run", "^TestContractToolSchemas$", "-count=1", "-v")
	res.Log["schemas"] = tail(out, 3000)
	if err != nil {
		res.ToolsNote = "the schemas could not be read from the new T3 (see the log)"
	} else if newTools, rerr := readJSON(toolsOut); rerr != nil {
		res.ToolsNote = "the schemas could not be read from the new T3: " + rerr.Error()
	} else if oldTools, rerr := readJSON(filepath.Join(o.Root, "t3", "contract", "tools.json")); rerr != nil {
		res.ToolsNote = "the snapshot could not be read: " + rerr.Error()
	} else {
		res.Changes = DiffTools(oldTools, newTools, o.UsedTools)
	}

	// 3. the contract tests against the new T3
	out, err = o.Runner.Run(ctx, o.Root, env, "go", "test", "./internal/t3", "-run", "Contract", "-count=1", "-v", "-timeout", "30m")
	res.Log["contract"] = tail(out, 4000)
	if err != nil {
		res.Contract = "failed"
	} else {
		res.Contract = "passed"
	}
	return res, nil
}

func readJSON(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// Report is the pull request's text.
func Report(r Result) string {
	var b strings.Builder
	if !r.Changed {
		fmt.Fprintf(&b, "# T3 pin is up to date\n\nThe newest nightly is `%s`, which is the pin. Nothing changed.\n", r.Old.Tag)
		return b.String()
	}
	fmt.Fprintf(&b, "# Move the T3 pin to `%s`\n\n", r.New.Tag)
	fmt.Fprintf(&b, "| | before | after |\n|---|---|---|\n| version | `%s` | `%s` |\n| commit | `%s` | `%s` |\n| npm integrity | `%s` | `%s` |\n| checked | %s | %s |\n\n",
		r.Old.Version, r.New.Version, short(r.Old.Commit), short(r.New.Commit), short(r.Old.Integrity), short(r.New.Integrity), r.Old.Checked, r.New.Checked)
	b.WriteString("Release archives (SHA-256, from the release's `SHA256SUMS`):\n\n")
	plats := make([]string, 0, len(r.New.Runtime.SHA256))
	for p := range r.New.Runtime.SHA256 {
		plats = append(plats, p)
	}
	sort.Strings(plats)
	for _, p := range plats {
		fmt.Fprintf(&b, "- `%s` `%s`\n", p, r.New.Runtime.SHA256[p])
	}
	b.WriteString("\nThe npm integrity was checked by downloading the tarball the registry points to and hashing it.\n\n")
	fmt.Fprintf(&b, "## Checks\n\n- install of the pin into a temporary folder (hash-checked): **%s**\n- contract tests against it: **%s**\n\n", r.Install, r.Contract)
	b.WriteString("## Tool schemas Rubi uses\n\n")
	switch {
	case r.ToolsNote != "":
		fmt.Fprintf(&b, "%s\n\n", r.ToolsNote)
	case r.Install == "skipped":
		b.WriteString("Not compared (the contract run was skipped).\n\n")
	case len(r.Changes) == 0:
		b.WriteString("No input schema of the tools Rubi uses changed.\n\n")
	default:
		fmt.Fprintf(&b, "%d change(s) in the input schemas of the tools Rubi uses. `t3/contract/tools.json` is **not** updated by this pull request; "+
			"review each change, then run the contract test with `RUBI_T3_UPDATE=1` on this branch to take it.\n\n", len(r.Changes))
		for _, c := range r.Changes {
			b.WriteString("- " + c.String() + "\n")
		}
		b.WriteString("\n")
	}
	if r.Contract == "failed" || r.Install == "failed" {
		b.WriteString("## Log\n\n")
		keys := make([]string, 0, len(r.Log))
		for k := range r.Log {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if r.Log[k] != "" && (k == "install" && r.Install == "failed" || k == "contract" && r.Contract == "failed") {
				fmt.Fprintf(&b, "<details><summary>%s</summary>\n\n```\n%s\n```\n</details>\n\n", k, r.Log[k])
			}
		}
	}
	b.WriteString("This pull request is never merged by a program. Taking a new T3 also means running the live end-to-end tests " +
		"(`RUBI_T3VM_LIVE=1`) on a computer with the container tool, and reading the release notes.\n")
	return b.String()
}

func short(s string) string {
	if len(s) > 19 {
		return s[:19] + "…"
	}
	return s
}

// Today is the date for the "checked" field.
func Today() string { return time.Now().UTC().Format("2006-01-02") }

var _ = errors.New
