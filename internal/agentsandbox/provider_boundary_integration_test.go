package agentsandbox

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These are execution-boundary probes, NOT Cursor/OpenCode implementations or
// proof that T3 is connected. They use no account, real provider, or live daemon.
func providerBoundaryFixture(t *testing.T) (Launch, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux network-namespace probe")
	}
	srt, err := exec.LookPath("srt")
	if err != nil {
		t.Skip("srt unavailable")
	}
	p, settings := fixture(t)
	root := filepath.Dir(settings)
	p.SensitiveRoots = []string{root}
	real, err := filepath.EvalSymlinks(srt)
	if err != nil {
		t.Fatal(err)
	}
	p.RuntimeRead = []string{filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(real))))}
	var search []string
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(d) {
			if st, err := os.Stat(d); err == nil && st.IsDir() {
				search = append(search, d)
				if strings.HasPrefix(d, "/workspace/") {
					p.RuntimeRead = append(p.RuntimeRead, d)
				}
			}
		}
	}
	other := filepath.Join(p.Home, "project-b")
	for _, d := range []string{other, filepath.Join(p.Home, ".ssh")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(other, "marker"), filepath.Join(p.Home, ".ssh", "fixture-secret")} {
		if err := os.WriteFile(path, []byte("disposable fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return Launch{Policy: p, SettingsDir: settings, SRT: srt, SearchPath: search}, other
}

func TestInProcessHostBoundarySRTProbe(t *testing.T) {
	l, other := providerBoundaryFixture(t)
	l.OwnProcessTree = true
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	l.CLI = node
	// This fake models direct fs + spawned-shell work in the SDK host process.
	// No cursor-agent binary is invoked, just as T3's current SDK path doesn't invoke one.
	script := `const fs=require('fs'),cp=require('child_process'),assert=require('assert');
const [project,other,home,network,uid]=process.argv.slice(1);
assert.equal(process.getuid(),Number(uid),"provider UID changed (root-refusing CLIs would break)");
const status=fs.readFileSync('/proc/self/status','utf8');
for(const field of ['CapInh','CapPrm','CapEff','CapAmb']) {
 const match=status.match(new RegExp('^'+field+':\\s*([0-9a-f]+)$','m'));
 assert(match && BigInt('0x'+match[1])===0n,'provider inherited capabilities: '+field);
}
assert.equal(process.cwd(),project);
for(const path of [other+'/marker',home+'/.ssh/fixture-secret']) {
 assert(!fs.existsSync(path),'outside file visible');
}
fs.writeFileSync(project+'/sdk-edit','ok');
cp.execFileSync('/bin/sh',['-c','printf child > "$1/sdk-shell"','sh',project]);
for(const path of [other+'/sdk-write',home+'/.ssh/sdk-write']) {
 try { fs.writeFileSync(path,'sandbox-shadow'); } catch(e) {}
}
if(network==='1') {
 const result=cp.spawnSync('curl',['-sS','--max-time','5','https://example.com']);
 assert.notEqual(result.status,0,'network deny bypassed');
 assert(result.stderr.toString().includes('403'),'not an allow-list refusal');
}
process.stdout.write('in-process-boundary-ok');`
	l.Args = []string{"-e", script, l.Policy.Projects[0], other, l.Policy.Home, os.Getenv("RUBI_SRT_NETWORK_TEST"), strconv.Itoa(os.Getuid())}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out, stderr bytes.Buffer
	code, err := Run(ctx, l, nil, &out, &stderr)
	if err != nil || code != 0 || out.String() != "in-process-boundary-ok" || stderr.Len() != 0 {
		t.Fatalf("boundary probe: code=%d err=%v stdout=%q stderr=%q", code, err, out.String(), stderr.String())
	}
	for _, name := range []string{"sdk-edit", "sdk-shell"} {
		if _, err := os.Stat(filepath.Join(l.Policy.Projects[0], name)); err != nil {
			t.Fatal("assigned project write missing", err)
		}
	}
	for _, path := range []string{filepath.Join(other, "sdk-write"), filepath.Join(l.Policy.Home, ".ssh", "sdk-write")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("outside write reached host")
		}
	}
}

func TestOpenCodePrivateLoopbackBoundarySRTProbe(t *testing.T) {
	l, other := providerBoundaryFixture(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	l.CLI = python
	// Holding the same numeric port on the host proves that the inner server
	// has its own loopback; a binaryPath shim cannot export it to T3's host client.
	hostListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := hostListener.Addr().(*net.TCPAddr).Port
	hostServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "host-fixture") }), ReadHeaderTimeout: time.Second}
	defer hostServer.Close()
	go hostServer.Serve(hostListener)
	script := `import os,sys,threading,http.server,urllib.request,subprocess
port,project,other,home,network=int(sys.argv[1]),*sys.argv[2:]
class Handler(http.server.BaseHTTPRequestHandler):
 def do_GET(self):
  self.send_response(200);self.end_headers();self.wfile.write(b'private-opencode-health')
 def log_message(self,*args): pass
server=http.server.ThreadingHTTPServer(('127.0.0.1',port),Handler)
threading.Thread(target=server.serve_forever,daemon=True).start()
local=urllib.request.build_opener(urllib.request.ProxyHandler({}))
assert local.open('http://127.0.0.1:'+str(port),timeout=2).read()==b'private-opencode-health'
for path in [other+'/marker',home+'/.ssh/fixture-secret']:
 assert not os.path.exists(path),'outside file visible'
open(project+'/opencode-edit','w').write('ok')
for path in [other+'/opencode-write',home+'/.ssh/opencode-write']:
 try: open(path,'w').write('sandbox-shadow')
 except OSError: pass
if network=='1':
 denied=subprocess.run(['curl','-sS','--max-time','5','https://example.com'],capture_output=True)
 assert denied.returncode!=0 and b'403' in denied.stderr,'network deny bypassed'
print('private-endpoint-ready',flush=True)
sys.stdin.readline()
server.shutdown()
`
	l.Args = []string{"-u", "-c", script, strconv.Itoa(port), l.Policy.Projects[0], other, l.Policy.Home, os.Getenv("RUBI_SRT_NETWORK_TEST")}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	inR, inW := io.Pipe()
	defer inW.Close()
	outR, outW := io.Pipe()
	defer outR.Close()
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	var stderr bytes.Buffer
	go func() {
		defer close(done)
		code, err := Run(ctx, l, inR, outW, &stderr)
		outW.CloseWithError(err)
		done <- result{code, err}
	}()
	defer func() {
		inW.Close()
		cancel()
		outR.Close()
		for range done {
		}
	}()
	ready, err := bufio.NewReader(outR).ReadString('\n')
	if err != nil || ready != "private-endpoint-ready\n" {
		inW.Close()
		r := <-done
		t.Fatalf("inner startup: %v code=%d err=%v stderr=%q", err, r.code, r.err, stderr.String())
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(data) != "host-fixture" {
		t.Fatal("inner namespace replaced/exposed host endpoint")
	}
	// A second project's sandbox must not reach either the inner A server or
	// the host's fixture at the identical numeric endpoint.
	b := l
	b.Policy.Projects = []string{other}
	root := filepath.Dir(l.SettingsDir)
	b.Policy.Login = filepath.Join(root, "b-login")
	b.Policy.Tmp = filepath.Join(root, "b-tmp")
	b.SettingsDir = filepath.Join(root, "b-settings")
	for _, d := range []string{b.Policy.Login, b.Policy.Tmp, b.SettingsDir} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	b.Args = []string{"-c", `import os,sys,urllib.request
own,other,home=sys.argv[2:]
assert os.path.exists(own+'/marker'),'own project hidden'
assert not os.path.exists(other+'/opencode-edit'),'other project readable'
assert not os.path.exists(home+'/.ssh/fixture-secret'),'HOME secret visible'
for path in [other+'/from-b',home+'/.ssh/from-b']:
 try: open(path,'w').write('sandbox-shadow')
 except OSError: pass
open(own+'/b-owned','w').write('ok')
local=urllib.request.build_opener(urllib.request.ProxyHandler({}))
try: local.open('http://127.0.0.1:'+sys.argv[1],timeout=2)
except OSError: pass
else: raise AssertionError('another namespace endpoint reachable')
import os
os.environ['NO_PROXY']='';os.environ['no_proxy']=''
try: urllib.request.urlopen('http://127.0.0.1:'+sys.argv[1],timeout=2)
except urllib.error.HTTPError as e: assert e.code==403
else: raise AssertionError('host endpoint reachable through proxy')
print('other-project-blocked',end='')`, strconv.Itoa(port), other, l.Policy.Projects[0], l.Policy.Home}
	var bOut, bErr bytes.Buffer
	code, err := Run(ctx, b, nil, &bOut, &bErr)
	if err != nil || code != 0 || bOut.String() != "other-project-blocked" || bErr.Len() != 0 {
		t.Fatalf("cross-project endpoint: code=%d err=%v stdout=%q stderr=%q", code, err, bOut.String(), bErr.String())
	}
	inW.Close()
	r := <-done
	if r.err != nil || r.code != 0 || stderr.Len() != 0 {
		t.Fatalf("inner shutdown: code=%d err=%v stderr=%q", r.code, r.err, stderr.String())
	}
	for _, path := range []string{filepath.Join(l.Policy.Projects[0], "opencode-edit"), filepath.Join(other, "b-owned")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("own project write missing", err)
		}
	}
	for _, path := range []string{filepath.Join(other, "opencode-write"), filepath.Join(l.Policy.Projects[0], "from-b"), filepath.Join(l.Policy.Home, ".ssh", "opencode-write"), filepath.Join(l.Policy.Home, ".ssh", "from-b")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("outside write reached host")
		}
	}
}
