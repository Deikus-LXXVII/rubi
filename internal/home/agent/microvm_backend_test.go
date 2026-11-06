//go:build unix

package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/microvm"
)

type tarEntry struct {
	name string
	body string
	mode int64
	kind byte // 0: regular file
	link string
}

func tarGz(t *testing.T, entries []tarEntry) []byte {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg, Linkname: e.link}
		if e.mode == 0 {
			h.Mode = 0o644
		}
		if e.kind != 0 {
			h.Typeflag, h.Size = e.kind, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

// release is a fake release of the VM files, served over HTTP, with sums signed by a throwaway key that the test build
// trusts (integrity's RUBI_TEST_RELEASE_KEY_FILE and RUBI_DOWNLOAD_BASE, for a "-test" version).
type release struct {
	t       *testing.T
	version string
	files   map[string][]byte
	served  map[string][]byte // what the server sends instead of files[name] (a tampered archive)
	key     ed25519.PrivateKey
	sigKey  ed25519.PrivateKey // signs the sums (key unless a test swaps it)
	srv     *httptest.Server
}

func newRelease(t *testing.T, arch microvm.Arch) *release {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	keyFile := filepath.Join(t.TempDir(), "key.pem")
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600)
	t.Setenv("RUBI_TEST_RELEASE_KEY_FILE", keyFile)
	r := &release{t: t, version: "v9.9.9-test", files: map[string][]byte{}, served: map[string][]byte{}, key: priv, sigKey: priv}
	kernel := "vmlinux-x86_64"
	if arch == microvm.AArch64 {
		kernel = "Image-aarch64"
	}
	q, v := "qemu-"+string(arch), "virtiofsd-"+string(arch)
	parts := map[string][]tarEntry{
		"kernel":    {{name: kernel, body: "kernel"}, {name: "config-" + string(arch), body: "CONFIG_PVH=y\n"}},
		"rootfs":    {{name: "rootfs.img", body: "ext4"}, {name: "rootfs.env", body: "PATH=/usr/local/bin:/usr/bin:/bin\nHOME=/tmp\n"}},
		"qemu":      {{name: q + "/bin/qemu-system-" + string(arch), body: "qemu", mode: 0o755}, {name: q + "/share/qemu/qboot.rom", body: "rom"}, {name: q + "/lib/libglib-2.0.so.0", body: "lib"}},
		"virtiofsd": {{name: v + "/bin/virtiofsd", body: "vfs", mode: 0o755}, {name: v + "/lib/libseccomp.so.2", body: "lib"}},
	}
	for part, e := range parts {
		r.files[fmt.Sprintf("rubi-vm-%s-linux-%s-%s.tar.gz", r.version, arch, part)] = tarGz(t, e)
	}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, "/"+r.version+"/")
		switch name {
		case "SHA256SUMS":
			_, _ = w.Write(r.sums())
		case "SHA256SUMS.sig":
			_, _ = w.Write(ed25519.Sign(r.sigKey, r.sums()))
		default:
			b, ok := r.files[name]
			if alt, tampered := r.served[name]; tampered {
				b = alt
			}
			if !ok {
				http.NotFound(w, req)
				return
			}
			_, _ = w.Write(b)
		}
	}))
	t.Cleanup(r.srv.Close)
	t.Setenv("RUBI_DOWNLOAD_BASE", r.srv.URL)
	return r
}

// sums is the release's SHA256SUMS, the same bytes every time (they are signed).
func (r *release) sums() []byte {
	names := make([]string, 0, len(r.files))
	for name := range r.files {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		sum := sha256.Sum256(r.files[name])
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	return []byte(b.String())
}

func TestMicroVMFilesInstall(t *testing.T) {
	r := newRelease(t, microvm.X86_64)
	f := microVMFiles{Root: t.TempDir(), Version: r.version, Arch: microvm.X86_64}
	if f.Present() {
		t.Fatal("present before the install")
	}
	var logged []string
	if err := f.Install(context.Background(), func(s string) { logged = append(logged, s) }); err != nil {
		t.Fatal(err)
	}
	if !f.Present() {
		t.Fatal("not present after the install")
	}
	for _, p := range []string{f.Kernel(), f.Rootfs(), f.QEMU(), f.Firmware(), f.Virtiofsd(), f.Libs()[0] + "/libseccomp.so.2"} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%v", err)
		}
	}
	if fi, _ := os.Stat(f.QEMU()); fi.Mode().Perm() != 0o755 {
		t.Errorf("qemu mode %v", fi.Mode().Perm())
	}
	env, err := f.LoadImageEnv()
	if err != nil || env["HOME"] != "/tmp" || !strings.HasPrefix(env["PATH"], "/usr/local/bin") {
		t.Errorf("image env %v %v", env, err)
	}
	if len(logged) == 0 {
		t.Error("nothing logged")
	}
}

// The setup's tool step installs the files through the provisioner (ToolInstaller), and is done after it.
func TestMicroVMToolStepInstallsTheFiles(t *testing.T) {
	r := newRelease(t, microvm.X86_64)
	b := &MicroVMBackend{files: microVMFiles{Root: t.TempDir(), Version: r.version, Arch: microvm.X86_64}}
	p := b.Provision()
	if st := p.Tool(context.Background(), nil, ""); st.State != StepTodo || st.Action != "install" {
		t.Fatalf("%+v", st)
	}
	ti, ok := p.(ToolInstaller)
	if !ok {
		t.Fatal("not a ToolInstaller")
	}
	if err := ti.InstallTool(context.Background(), func(string) {}); err != nil {
		t.Fatal(err)
	}
	if st := p.Tool(context.Background(), nil, ""); st.State != StepDone {
		t.Fatalf("%+v", st)
	}
}

// Nothing that doesn't check out is installed: a tampered archive, a signature by another key, a missing part, an archive
// with a link or a path that leaves its folder.
func TestMicroVMFilesInstallRefuses(t *testing.T) {
	name := func(r *release, part string) string {
		return fmt.Sprintf("rubi-vm-%s-linux-x86_64-%s.tar.gz", r.version, part)
	}
	for what, spoil := range map[string]func(r *release){
		"a tampered archive": func(r *release) { r.served[name(r, "qemu")] = tarGz(r.t, []tarEntry{{name: "evil", body: "x"}}) },
		"another key":        func(r *release) { _, r.sigKey, _ = ed25519.GenerateKey(rand.Reader) },
		"a missing part":     func(r *release) { delete(r.files, name(r, "virtiofsd")) },
		"a link": func(r *release) {
			r.files[name(r, "kernel")] = tarGz(r.t, []tarEntry{{name: "vmlinux-x86_64", kind: tar.TypeSymlink, link: "/etc/passwd"}})
		},
		"a path out": func(r *release) {
			r.files[name(r, "rootfs")] = tarGz(r.t, []tarEntry{{name: "../../escaped", body: "x"}})
		},
		"an absolute path": func(r *release) {
			r.files[name(r, "rootfs")] = tarGz(r.t, []tarEntry{{name: "/tmp/escaped", body: "x"}})
		},
	} {
		r := newRelease(t, microvm.X86_64)
		spoil(r)
		root := t.TempDir()
		f := microVMFiles{Root: root, Version: r.version, Arch: microvm.X86_64}
		if err := f.Install(context.Background(), func(string) {}); err == nil {
			t.Errorf("%s: installed", what)
		}
		if f.Present() {
			t.Errorf("%s: present", what)
		}
		ents, _ := os.ReadDir(root)
		for _, e := range ents {
			t.Errorf("%s: left %s", what, e.Name())
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escaped")); err == nil {
			t.Errorf("%s: wrote outside", what)
		}
	}
}

func testBackend(t *testing.T, kvmOK bool, kvmReason string, landlock error) *MicroVMBackend {
	b := NewMicroVMBackend(BackendConfig{EnvDir: t.TempDir()}, "v9.9.9-test", "/opt/rubi/rubi-home")
	b.files.Arch = microvm.X86_64
	b.kvm = func() (bool, string) { return kvmOK, kvmReason }
	b.landlock = func() error { return landlock }
	return b
}

func TestMicroVMBackendAvailable(t *testing.T) {
	for name, c := range map[string]struct {
		kvm      bool
		reason   string
		landlock error
		ok       bool
		want     string
	}{
		"ready":                  {true, "", nil, true, ""},
		"KVM not yet the user's": {false, microvm.ReasonKVMDenied, nil, true, ""},
		"no virtualisation":      {false, microvm.ReasonNoVirt, nil, false, microvm.ReasonNoVirt},
		"no /dev/kvm":            {false, microvm.ReasonNoKVM, nil, false, microvm.ReasonNoKVM},
		"no Landlock":            {true, "", microvm.ErrNoLandlock, false, ReasonNoLandlock},
	} {
		av := testBackend(t, c.kvm, c.reason, c.landlock).Available(context.Background())
		if av.OK != c.ok || av.Reason != c.want {
			t.Errorf("%s: %+v", name, av)
		}
	}
	b := testBackend(t, true, "", nil)
	b.files.Arch = ""
	if av := b.Available(context.Background()); av.OK || av.Reason != microvm.ReasonNoArch {
		t.Errorf("no guest CPU: %+v", av)
	}
}

// The wizard's steps: the files to install (then done), KVM ready or why not (no button: only an administrator can fix
// it), and the root file system with the files.
func TestMicroVMProvisioner(t *testing.T) {
	r := newRelease(t, microvm.X86_64)
	b := testBackend(t, false, microvm.ReasonKVMDenied, nil)
	b.files.Version = r.version
	p := b.Provision()
	if st := p.Tool(context.Background(), nil, ""); st.State != StepTodo || st.Action != "install" {
		t.Errorf("tool %+v", st)
	}
	if st := p.Image(context.Background(), nil); st.State != StepTodo {
		t.Errorf("image %+v", st)
	}
	if st := p.Service(context.Background(), nil); st.State != StepFailed || st.Action != "" || !strings.Contains(st.Reason, "kvm group") {
		t.Errorf("service %+v", st)
	}
	if err := p.StartService(context.Background(), nil, func(string) {}); err == nil {
		t.Error("started KVM")
	}
	if prepared, err := p.BuildImage(context.Background(), nil, func(string) {}); !prepared || err != nil {
		t.Fatalf("install: %v %v", prepared, err)
	}
	if st := p.Tool(context.Background(), nil, ""); st.State != StepDone {
		t.Errorf("tool after install %+v", st)
	}
	if st := p.Image(context.Background(), nil); st.State != StepDone {
		t.Errorf("image after install %+v", st)
	}
	b.kvm = func() (bool, string) { return true, "" }
	if st := p.Service(context.Background(), nil); st.State != StepDone {
		t.Errorf("service %+v", st)
	}
}

// A VM of the backend starts only with its files installed and KVM usable, and says which is missing.
func TestMicroVMBackendVMNeedsItsFilesAndKVM(t *testing.T) {
	b := testBackend(t, true, "", nil)
	vm := b.New(roleProject(t)).(*MicroVM)
	launched := false
	vm.launch = func(microvm.Spec) (microVMProcess, error) { launched = true; return nil, errors.New("no") }
	_, err := vm.Start(context.Background(), StartSpec{Project: roleProject(t), Pipe: &PipeSpec{}})
	if err == nil || !strings.Contains(err.Error(), "VM files") || launched {
		t.Errorf("without files: %v (launched %v)", err, launched)
	}
	if vm.Launcher.Confine[0] != "/opt/rubi/rubi-home" || vm.Launcher.Confine[1] != "vm-confine" || vm.Arch != microvm.X86_64 {
		t.Errorf("vm %+v", vm.Launcher)
	}
}
