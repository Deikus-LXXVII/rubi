package agentsandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// An installed, disposable release staging tree, never the live installation.
func TestBundledRuntimeSRTIntegration(t *testing.T) {
	exe := os.Getenv("RUBI_BUNDLED_RUNTIME_TEST")
	if exe == "" {
		t.Skip("set RUBI_BUNDLED_RUNTIME_TEST to a staged rubi-home executable")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Unix runtime")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	p, settings := fixture(t)
	p.SensitiveRoots = []string{filepath.Dir(settings)}
	secret := filepath.Join(p.Home, "fake-secret")
	if err := os.WriteFile(secret, []byte("fixture-not-a-real-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(settings), "project-b")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "marker"), []byte("fixture-b"), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := ResolveRuntime(Launch{Policy: p, SettingsDir: settings, CLI: python, SearchPath: []string{"/usr/bin", "/bin"}}, exe)
	if err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(runtimeRoot(exe), "bin/node")
	var pin struct{ Node string }
	data, err := runtimePins.ReadFile("runtime/sources.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &pin); err != nil {
		t.Fatal(err)
	}
	probe := `import os,sys,subprocess
project,other,secret,node,expected=sys.argv[1:]
for f in [other+'/marker',secret]:
 try:open(f).read()
 except OSError:pass
 else:raise AssertionError('outside fixture readable')
for f in [other+'/written',node]:
 try:open(f,'wb').write(b'tamper')
 except OSError:pass
 else:raise AssertionError('protected path writable')
open(project+'/passed','w').write('ok')
assert subprocess.check_output(['node','--version']).decode().strip()==expected
print('bundled-runtime-ok')
`
	l.Args = []string{"-c", probe, p.Projects[0], other, secret, node, pin.Node}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out, stderr bytes.Buffer
	code, err := Run(ctx, l, nil, &out, &stderr)
	if err != nil || code != 0 || out.String() != "bundled-runtime-ok\n" {
		t.Fatalf("bundled runtime: code=%d err=%v out=%q stderr=%q", code, err, out.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(p.Projects[0], "passed")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "written")); !os.IsNotExist(err) {
		t.Fatal("outside write reached host")
	}
}
