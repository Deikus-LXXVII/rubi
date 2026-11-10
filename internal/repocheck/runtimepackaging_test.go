package repocheck

import (
	"archive/zip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeBuilderRefusesUnpinnedAndEscapingInputs(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python release tooling unavailable")
	}
	script := `
import runpy,pathlib,tempfile,tarfile,io,hashlib,json
repo=pathlib.Path(__import__('sys').argv[1]);builder=repo/'scripts/srt-runtime/build.py';b=runpy.run_path(str(builder))
pins=json.loads((repo/'internal/agentsandbox/runtime/sources.json').read_text())
for name,want in pins['recipes'].items():
 assert hashlib.sha256((builder.parent/name).read_bytes()).hexdigest()==want,'unpinned build recipe'
with tempfile.TemporaryDirectory() as tmp:
 p=pathlib.Path(tmp);cache=p/'cache';cache.mkdir();(cache/'asset.tar.gz').write_bytes(b'fixture')
 try:b['download']({'url':'https://invalid.example/asset.tar.gz','sha256':'0'*64},cache)
 except ValueError:pass
 else:raise AssertionError('checksum mismatch accepted')
 archive=p/'escape.tar'
 with tarfile.open(archive,'w') as t:
  i=tarfile.TarInfo('../outside');i.size=1;t.addfile(i,io.BytesIO(b'x'))
 dest=p/'extract';dest.mkdir()
 try:b['unpack'](archive,dest)
 except tarfile.FilterError:pass
 else:raise AssertionError('unsafe extraction accepted')
 assert not (p/'outside').exists()
 try:b['stage']('windows-amd64',p/'out',cache,'cc')
 except ValueError:pass
 else:raise AssertionError('Windows sandbox accepted')
 out=p/'out';target=out/'srt-runtime'/b['identity']()/'linux-amd64';target.mkdir(parents=True);(target/'keep').write_text('untouched')
 try:b['stage']('linux-amd64',out,cache,'cc')
 except ValueError:pass
 else:raise AssertionError('existing runtime overwritten')
 assert (target/'keep').read_text()=='untouched'
`
	cmd := exec.Command(python, "-c", script, root(t))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("builder safety: %v\n%s", err, out)
	}
}

func TestHomeAppCarriesSandboxCompanions(t *testing.T) {
	if _, err := exec.LookPath("zip"); err != nil {
		t.Skip("zip unavailable")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "rubi-home")
	files := []string{"rubi-home", "rubi-agent-exec", "srt-runtime/pin/darwin-arm64/bin/node", "srt-runtime/pin/darwin-amd64/bin/node"}
	for _, name := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("fixture"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	outdir := filepath.Join(dir, "out")
	cmd := exec.Command("sh", filepath.Join(root(t), "scripts/build-home-app.sh"), "v1.2.3", "universal", binary, outdir)
	cmd.Env = append(os.Environ(), "RUBI_APP_SIGN=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("app packaging: %v\n%s", err, out)
	}
	z, err := zip.OpenReader(filepath.Join(outdir, "rubi-home-v1.2.3-macos-universal.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	names := map[string]bool{}
	for _, f := range z.File {
		names[f.Name] = true
	}
	for _, name := range files {
		if !names["Rubi Home.app/Contents/MacOS/"+name] {
			t.Fatal("missing companion", name)
		}
	}
	if err := os.Remove(filepath.Join(dir, "rubi-agent-exec")); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("sh", filepath.Join(root(t), "scripts/build-home-app.sh"), "v1.2.3", "universal", binary, outdir)
	cmd.Env = append(os.Environ(), "RUBI_APP_SIGN=")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "launcher is missing") {
		t.Fatal("incomplete runtime app built", err)
	}
}

func TestMacRuntimeManifestMatchesSignedHelperBytes(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python release tooling unavailable")
	}
	script := `
import pathlib,tempfile,subprocess,os,hashlib,sys
with tempfile.TemporaryDirectory() as tmp:
 p=pathlib.Path(tmp);app=p/'Rubi Home.app';rt=app/'Contents/MacOS/srt-runtime/pin/darwin-arm64';(rt/'bin').mkdir(parents=True)
 node=rt/'bin/node';node.write_bytes(bytes.fromhex('cffaedfe')+b'fixture');(rt/'COMPLETE').write_text('pin')
 fake=p/'fake';fake.mkdir();cs=fake/'codesign';cs.write_text('#!/bin/sh\ncase \" $* \" in *\" --preserve-metadata=entitlements,flags,runtime \"*) ;; *) exit 91;; esac\nfor arg do last=$arg; done\nprintf sealed >> "$last"\n');cs.chmod(0o755)
 env=dict(os.environ,PATH=str(fake)+os.pathsep+os.environ['PATH'])
 subprocess.run([sys.executable,str(pathlib.Path(sys.argv[1])/'scripts/srt-runtime/seal-macos.py'),str(app)],check=True,env=env)
 assert node.read_bytes().endswith(b'sealed'),'nested helper not signed'
 want=hashlib.sha256(node.read_bytes()).hexdigest()+'  bin/node\n'
 assert (rt/'SHA256SUMS').read_text()==want,'manifest hashes unsigned bytes'
`
	cmd := exec.Command(python, "-c", script, root(t))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("manifest sealing: %v\n%s", err, out)
	}
}

// codesign validates the code nested under an executable in Contents/MacOS, so the
// runtime helpers must be sealed before rubi-home; v0.15.0's release failed on this order.
func TestMacSealSignsNestedHelpersBeforeTopLevelExecutables(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python release tooling unavailable")
	}
	script := `
import pathlib,tempfile,subprocess,os,sys
with tempfile.TemporaryDirectory() as tmp:
 p=pathlib.Path(tmp);app=p/'Rubi Home.app';mac=app/'Contents/MacOS';rt=mac/'srt-runtime/pin/darwin-amd64';(rt/'bin').mkdir(parents=True)
 for f in [mac/'rubi-home',mac/'rubi-agent-exec',rt/'bin/node',rt/'bin/rg']: f.write_bytes(bytes.fromhex('cffaedfe')+b'fixture')
 (rt/'COMPLETE').write_text('pin')
 fake=p/'fake';fake.mkdir();cs=fake/'codesign'
 cs.write_text('#!/bin/sh\nfor arg do last=$arg; done\ncase "$last" in */srt-runtime/*) ;; *) for h in "'+str(rt)+'"/bin/*; do tail -c 6 "$h" | grep -q sealed || { echo "nested $h unsigned" >&2; exit 1; }; done;; esac\nprintf sealed >> "$last"\n');cs.chmod(0o755)
 env=dict(os.environ,PATH=str(fake)+os.pathsep+os.environ['PATH'])
 subprocess.run([sys.executable,str(pathlib.Path(sys.argv[1])/'scripts/srt-runtime/seal-macos.py'),str(app)],check=True,env=env)
`
	cmd := exec.Command(python, "-c", script, root(t))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seal order: %v\n%s", err, out)
	}
}
