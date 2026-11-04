package t3bump

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeRunner stands in for the go test commands: it records them and plays their effects.
type fakeRunner struct {
	mu          sync.Mutex
	calls       []call
	installFail bool
	schemaFail  bool
	contractErr bool
	newTools    string // what the "new T3" gives for the tools
	noBin       bool
}

type call struct {
	dir  string
	env  map[string]string
	args []string
}

func (f *fakeRunner) Run(_ context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		e[k] = v
	}
	f.calls = append(f.calls, call{dir, e, append([]string{name}, args...)})
	run := strings.Join(args, " ")
	switch {
	case strings.Contains(run, "TestLiveT3Install"):
		if f.installFail {
			return []byte("install failed: checksum mismatch"), errors.New("exit 1")
		}
		if !f.noBin {
			d := e["RUBI_T3_INSTALL_DIR"]
			os.MkdirAll(filepath.Join(d, "t3", "runtime", "node", "bin"), 0o755)
			os.MkdirAll(filepath.Join(d, "t3", "runtime", "pkg", "bin"), 0o755)
			os.WriteFile(filepath.Join(d, "t3", "runtime", "node", "bin", "node"), []byte("x"), 0o755)
			os.WriteFile(filepath.Join(d, "t3", "runtime", "pkg", "bin", "t3.mjs"), []byte("x"), 0o755)
		}
		return []byte("--- PASS: TestLiveT3Install"), nil
	case strings.Contains(run, "TestContractToolSchemas"):
		if f.schemaFail {
			return []byte("could not start"), errors.New("exit 1")
		}
		os.WriteFile(e["RUBI_T3_TOOLS_OUT"], []byte(f.newTools), 0o644)
		return []byte("ok"), nil
	default:
		if f.contractErr {
			return []byte("--- FAIL: TestContractBesideTheUsersT3\n    contract_test.go:1: boom"), errors.New("exit 1")
		}
		return []byte("ok"), nil
	}
}

// repo makes a repository root with the pin and the snapshot of the T3 it is at now.
func repo(t *testing.T) string {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "t3", "contract"), 0o755)
	real, err := os.ReadFile(filepath.Join("..", "..", "t3", "upstream.json"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "t3", "upstream.json"), real, 0o644)
	os.WriteFile(filepath.Join(root, "t3", "contract", "tools.json"), []byte(`{"t3_thread_send":{"type":"object","properties":{"message":{"type":"string"}}}}`), 0o644)
	return root
}

func opts(t *testing.T, w *fakeWorld, root string, r Runner) Options {
	return Options{Root: root, Client: w.client(), Runner: r, Date: "2027-02-03", UsedTools: []string{"t3_thread_send", "t3_thread_read"},
		TempDir: func() (string, error) { return t.TempDir(), nil }}
}

const newTag = "v0.2.0-nightly.20270201.7"

func TestRunBumpsThePinInstallsItAndComparesTheSchemas(t *testing.T) {
	w := newWorld(t)
	w.add("v0.1.7-nightly.20261102.4093")
	w.add(newTag)
	root := repo(t)
	before, _ := Read(root)
	fr := &fakeRunner{newTools: `{"t3_thread_send":{"type":"object","properties":{"message":{"type":"string","maxLength":5}}},"t3_thread_read":{"type":"object"}}`}
	res, err := Run(context.Background(), opts(t, w, root, fr))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Install != "passed" || res.Contract != "passed" || res.New.Tag != newTag || res.Old.Tag != before.Tag {
		t.Fatalf("result: %+v", res)
	}
	// the file on disk is the new pin, dated, with the old node pin and license
	got, _ := Read(root)
	if got.Version != "0.2.0-nightly.20270201.7" || got.Checked != "2027-02-03" || got.Commit != w.commits[newTag] || got.Integrity != sri(w.tarballs[got.Version]) ||
		len(got.Runtime.SHA256) != len(Platforms) || got.License != before.License || string(got.Node) != string(before.Node) {
		t.Errorf("upstream.json: %+v", got)
	}
	// the commands: install first, with the folder; the schemas and the contract tests against the installed pin
	if len(fr.calls) != 3 {
		t.Fatalf("calls: %v", fr.calls)
	}
	in, sc, ct := fr.calls[0], fr.calls[1], fr.calls[2]
	if in.dir != root || in.env["RUBI_T3_INSTALL_LIVE"] != "1" || in.env["RUBI_T3_INSTALL_DIR"] == "" {
		t.Errorf("install: %+v", in)
	}
	dir := in.env["RUBI_T3_INSTALL_DIR"]
	for _, c := range []call{sc, ct} {
		if c.env["RUBI_T3_CONTRACT"] != "1" || c.env["RUBI_T3_NODE"] != filepath.Join(dir, "t3", "runtime", "node", "bin", "node") ||
			c.env["RUBI_T3_BIN"] != filepath.Join(dir, "t3", "runtime", "pkg", "bin", "t3.mjs") || c.env["RUBI_T3_RUNTIME_HOME"] != filepath.Join(dir, "t3", "runtime", "t3home") {
			t.Errorf("env: %v", c.env)
		}
	}
	if sc.env["RUBI_T3_TOOLS_OUT"] == "" || ct.env["RUBI_T3_TOOLS_OUT"] != "" || ct.env["RUBI_T3_UPDATE"] != "" || sc.env["RUBI_T3_UPDATE"] != "" {
		t.Errorf("the snapshot must not be rewritten: %v %v", sc.env, ct.env)
	}
	// the snapshot is left alone, and the changes are the new schemas against it
	snap, _ := os.ReadFile(filepath.Join(root, "t3", "contract", "tools.json"))
	if !strings.Contains(string(snap), `"message":{"type":"string"}`) {
		t.Errorf("tools.json was changed: %s", snap)
	}
	var lines []string
	for _, c := range res.Changes {
		lines = append(lines, c.Tool+" "+c.Path+" "+c.Kind)
	}
	if strings.Join(lines, ";") != "t3_thread_read (the tool) added;t3_thread_send properties.message.maxLength added" {
		t.Errorf("changes: %v", lines)
	}
	rep := Report(res)
	for _, want := range []string{"Move the T3 pin to `" + newTag + "`", "0.1.7-nightly.20261102.4093", "darwin-arm64", "contract tests against it: **passed**",
		"2 change(s)", "`t3_thread_send` `properties.message.maxLength` added", "`t3_thread_read` `(the tool)` added", "is **not** updated", "never merged by a program"} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
	if strings.Contains(rep, "<details>") {
		t.Error("a log in a passing report")
	}
}

func TestRunReportsAFailedContractAndAFailedInstall(t *testing.T) {
	w := newWorld(t)
	w.add(newTag)
	// contract fails: still a bump, with the log
	root := repo(t)
	fr := &fakeRunner{newTools: `{"t3_thread_send":{"type":"object","properties":{"message":{"type":"string"}}}}`, contractErr: true}
	res, err := Run(context.Background(), opts(t, w, root, fr))
	if err != nil {
		t.Fatal(err)
	}
	rep := Report(res)
	if res.Contract != "failed" || !res.Changed || !strings.Contains(rep, "contract tests against it: **failed**") || !strings.Contains(rep, "TestContractBesideTheUsersT3") ||
		!strings.Contains(rep, "No input schema of the tools Rubi uses changed") {
		t.Errorf("%+v\n%s", res, rep)
	}
	if got, _ := Read(root); got.Tag != newTag {
		t.Error("the pin was not written")
	}
	// install fails: the contract tests do not run
	root = repo(t)
	fr = &fakeRunner{installFail: true}
	res, err = Run(context.Background(), opts(t, w, root, fr))
	if err != nil || res.Install != "failed" || !strings.HasPrefix(res.Contract, "not run") || len(fr.calls) != 1 {
		t.Fatalf("%+v %v calls %d", res, err, len(fr.calls))
	}
	if rep := Report(res); !strings.Contains(rep, "install of the pin into a temporary folder (hash-checked): **failed**") || !strings.Contains(rep, "checksum mismatch") {
		t.Errorf("report:\n%s", rep)
	}
	// the install left no node
	fr = &fakeRunner{noBin: true}
	root = repo(t)
	if res, _ = Run(context.Background(), opts(t, w, root, fr)); res.Install != "failed" || len(fr.calls) != 1 {
		t.Errorf("no node: %+v", res)
	}
	// the schemas could not be read: said so, the contract tests still run
	fr = &fakeRunner{schemaFail: true}
	root = repo(t)
	res, _ = Run(context.Background(), opts(t, w, root, fr))
	if res.ToolsNote == "" || res.Contract != "passed" || len(fr.calls) != 3 || !strings.Contains(Report(res), "could not be read") {
		t.Errorf("schemas failed: %+v", res)
	}
}

func TestRunDoesNothingWhenThereIsNothingNew(t *testing.T) {
	w := newWorld(t)
	root := repo(t)
	cur, _ := Read(root)
	w.add(cur.Tag)
	fr := &fakeRunner{}
	res, err := Run(context.Background(), opts(t, w, root, fr))
	if err != nil || res.Changed || len(fr.calls) != 0 {
		t.Fatalf("%+v %v %v", res, err, fr.calls)
	}
	if !strings.Contains(Report(res), "up to date") {
		t.Error(Report(res))
	}
	b, _ := os.ReadFile(filepath.Join(root, "t3", "upstream.json"))
	orig, _ := os.ReadFile(filepath.Join("..", "..", "t3", "upstream.json"))
	if string(b) != string(orig) {
		t.Error("the file changed")
	}
}

func TestRunNeverMovesBackAndNeverWritesAHalfPin(t *testing.T) {
	root := repo(t)
	orig, _ := os.ReadFile(filepath.Join(root, "t3", "upstream.json"))
	// the newest published is older than the pin
	w := newWorld(t)
	w.add("v0.1.7-nightly.20261001.1")
	fr := &fakeRunner{}
	if _, err := Run(context.Background(), opts(t, w, root, fr)); err == nil || !strings.Contains(err.Error(), "older than the pin") {
		t.Errorf("older: %v", err)
	}
	// a newer one whose npm integrity is wrong: an error and the file is untouched
	w2 := newWorld(t)
	w2.add(newTag)
	w2.integr["0.2.0-nightly.20270201.7"] = sri([]byte("else"))
	if _, err := Run(context.Background(), opts(t, w2, root, fr)); err == nil {
		t.Error("a bad integrity was written")
	}
	// GitHub down
	w3 := newWorld(t)
	w3.status["/repos/"] = 500
	if _, err := Run(context.Background(), opts(t, w3, root, fr)); err == nil {
		t.Error("GitHub down")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "t3", "upstream.json")); string(b) != string(orig) || len(fr.calls) != 0 {
		t.Errorf("the file changed or a command ran: %d calls", len(fr.calls))
	}
}

func TestSkipContractWritesThePinOnly(t *testing.T) {
	w := newWorld(t)
	w.add(newTag)
	root := repo(t)
	fr := &fakeRunner{}
	o := opts(t, w, root, fr)
	o.SkipContract = true
	res, err := Run(context.Background(), o)
	if err != nil || !res.Changed || res.Install != "skipped" || res.Contract != "skipped" || len(fr.calls) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if rep := Report(res); !strings.Contains(rep, "Not compared (the contract run was skipped)") {
		t.Error(rep)
	}
	var _ = json.Marshal
}

// The workflow opens a pull request and nothing else: no merge, no auto-merge, and no token beyond the job's own.
func TestWorkflowOpensAPullRequestAndNeverMerges(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "t3-bump.yml"))
	if err != nil {
		t.Fatal(err)
	}
	w := string(b)
	for _, want := range []string{"schedule:", "workflow_dispatch:", "go run ./cmd/t3-bump", "gh pr create", "--body-file", "pull-requests: write"} {
		if !strings.Contains(w, want) {
			t.Errorf("workflow lacks %q", want)
		}
	}
	for _, bad := range []string{"gh pr merge", "--auto", "automerge", "auto-merge:", "enable-auto", "--admin", "secrets.PAT", "secrets.GH_PAT", "pull_request_target", "git push --force", "push -f"} {
		if strings.Contains(strings.ReplaceAll(w, "never merges", ""), bad) {
			t.Errorf("workflow has %q", bad)
		}
	}
	for _, line := range strings.Split(w, "\n") {
		if strings.Contains(line, "uses:") && !strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- ")), "uses: actions/") {
			t.Errorf("a third-party action: %s", line)
		}
	}
}
