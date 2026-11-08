package agentsandbox

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This test uses only disposable fixtures. Set RUBI_SRT_NETWORK_TEST=1 to
// additionally exercise the public network allow-list (requires internet).
func TestSRTIntegration(t *testing.T) {
	srt, e := exec.LookPath("srt")
	if e != nil {
		t.Skip("srt unavailable")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("unsupported platform")
	}
	for _, dep := range []string{"python3", "node", "rg"} {
		if _, e := exec.LookPath(dep); e != nil {
			t.Fatalf("integration prerequisite %s missing", dep)
		}
	}
	p, settings := fixture(t)
	root := filepath.Dir(settings)
	p.SensitiveRoots = []string{root}
	// Test installation is supplied by PATH, not by a production global lookup.
	dirs := filepath.SplitList(os.Getenv("PATH"))
	var search []string
	for _, d := range dirs {
		if filepath.IsAbs(d) {
			if _, e := os.Stat(d); e == nil {
				search = append(search, d)
			}
		}
	}
	resolved, e := filepath.EvalSymlinks(srt)
	if e != nil {
		t.Fatal(e)
	}
	// npm bin -> <package>/dist/cli.js; preserve the entire dependency tree.
	npmModules := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(resolved))))
	p.RuntimeRead = []string{npmModules}
	for _, d := range search {
		if strings.HasPrefix(d, "/workspace/") {
			p.RuntimeRead = append(p.RuntimeRead, d)
		}
	}
	python, e := exec.LookPath("python3")
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{filepath.Join(p.Home, "p2"), filepath.Join(p.Home, ".ssh")} {
		if e := os.MkdirAll(path, 0700); e != nil {
			t.Fatal(e)
		}
	}
	for _, path := range []string{filepath.Join(p.Home, "p2", "marker"), filepath.Join(p.Home, ".ssh", "fake-key"), filepath.Join(p.Home, "secret"), filepath.Join(p.Projects[0], "marker"), filepath.Join(p.Login, "marker")} {
		if e := os.WriteFile(path, []byte("fixture"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	probe := `import os,sys,socket
project,login,tmp,home,settings=sys.argv[1:]
for base in [project,login]:
 assert open(base+'/marker').read()=='fixture'
for base in [project,login,tmp]:
 open(base+'/written','w').write('ok')
for path in [home+'/p2/marker',home+'/.ssh/fake-key',home+'/secret',settings]:
 try: open(path).read()
 except (FileNotFoundError,PermissionError,IsADirectoryError): pass
 else: raise AssertionError('hidden path readable')
for path in [home+'/p2/outside',home+'/outside']:
 try: open(path,'w').write('not-on-host')
 except OSError: pass
if sys.platform=='linux':
 try: socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)
 except PermissionError: pass
 else: raise AssertionError('unix sockets not blocked')
for root in ['/home/box','/cursor']:
 if os.path.isdir(root): assert not os.listdir(root), 'host root visible'
# A known non-secret source fixture under the denied workspace must be hidden.
try: open('/workspace/srt-spike-host-probe.txt').read()
except (FileNotFoundError,PermissionError): pass
else: raise AssertionError('host workspace marker readable')
assert 'FAKE_SECRET' not in os.environ
assert 'NODE_OPTIONS' not in os.environ
sys.stdout.write(sys.stdin.read())
sys.stderr.write('probe-stderr')
sys.exit(17)
`
	t.Setenv("FAKE_SECRET", "fake")
	t.Setenv("NODE_OPTIONS", "--no-warnings")
	l := Launch{Policy: p, SettingsDir: settings, SRT: srt, CLI: python, SearchPath: search, Args: []string{"-c", probe, p.Projects[0], p.Login, p.Tmp, p.Home, settings}}
	var out, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code, e := Run(ctx, l, strings.NewReader("{\"spike\":true}\n"), &out, &stderr)
	if e != nil || code != 17 || out.String() != "{\"spike\":true}\n" || stderr.String() != "probe-stderr" {
		t.Fatalf("code=%d err=%v stdout=%q stderr=%q", code, e, out.String(), stderr.String())
	}
	for _, base := range []string{p.Projects[0], p.Login, p.Tmp} {
		if _, e := os.Stat(filepath.Join(base, "written")); e != nil {
			t.Fatal("allowed write did not reach host", e)
		}
	}
	for _, path := range []string{filepath.Join(p.Home, "p2", "outside"), filepath.Join(p.Home, "outside")} {
		if _, e := os.Stat(path); !os.IsNotExist(e) {
			t.Fatal("outside write reached host")
		}
	}
	t.Log("filesystem, private tmp (EROFS regression), fake secrets, env, unix sockets, stdio and exit 17 passed")
	if os.Getenv("RUBI_SRT_NETWORK_TEST") != "1" {
		t.Log("network opt-in not enabled")
		return
	}
	curl, e := exec.LookPath("curl")
	if e != nil {
		t.Fatal(e)
	}
	l.CLI = curl
	l.Policy.AllowedDomains = []string{"github.com"}
	for _, domain := range []string{"github.com", "example.com"} {
		t.Run(domain, func(t *testing.T) {
			l.Args = []string{"-I", "-sS", "--max-time", "15", "-o", "/dev/null", "-w", "%{http_code}", "https://" + domain}
			out.Reset()
			stderr.Reset()
			code, e := Run(ctx, l, nil, &out, &stderr)
			if e != nil {
				t.Fatal(e)
			}
			if domain == "github.com" && (code != 0 || out.String() != "200") {
				t.Fatalf("allow-listed request: code=%d out=%q err=%q", code, out.String(), stderr.String())
			}
			if domain == "example.com" && (code == 0 || !strings.Contains(stderr.String(), "403")) {
				t.Fatalf("denied request: code=%d out=%q err=%q", code, out.String(), stderr.String())
			}
		})
	}
}
