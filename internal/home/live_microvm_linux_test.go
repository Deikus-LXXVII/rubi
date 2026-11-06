package home

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/setup"
)

// liveMicroVMVersion is the version the live runs' VM files are served as: a "-test" one, so the test build trusts a
// throwaway key and a local server (integrity's RUBI_TEST_RELEASE_KEY_FILE and RUBI_DOWNLOAD_BASE).
const liveMicroVMVersion = "v0.0.0-test"

// liveMicroVMNewVM makes the live tests' VMs with the microvm backend on a Linux computer with KVM, or nil where they run on
// Apple's. It needs RUBI_LIVE_VM_ARCHIVES, a folder with this CPU's four VM archives (rubi-vm-<any
// version>-linux-<arch>-{kernel,rootfs,qemu,virtiofsd}.tar.gz, as the microvm workflow makes them), and
// RUBI_LIVE_HOME_BIN, a rubi-home binary for its vm-confine command. The files are installed the way a user's are: the
// setup wizard's tool step downloads them from a release whose SHA256SUMS is signed (here by a key made for the run and
// served from this process). Each VM gets RUBI_LIVE_VM_MEMORY (256M by default). After the test no QEMU or file server
// of the run may be left.
func liveMicroVMNewVM(t *testing.T, h *Helper) func(agent.Project) agent.VM {
	dir, bin := os.Getenv("RUBI_LIVE_VM_ARCHIVES"), os.Getenv("RUBI_LIVE_HOME_BIN")
	if dir == "" || bin == "" {
		t.Fatal("on Linux the live runs need RUBI_LIVE_VM_ARCHIVES and RUBI_LIVE_HOME_BIN")
	}
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	files := map[string][]byte{}
	for _, part := range []string{"kernel", "rootfs", "qemu", "virtiofsd"} {
		m, _ := filepath.Glob(filepath.Join(dir, "rubi-vm-*-linux-"+arch+"-"+part+".tar.gz"))
		if len(m) != 1 {
			t.Fatalf("%s archive in %s: %v", part, dir, m)
		}
		b, err := os.ReadFile(m[0])
		if err != nil {
			t.Fatal(err)
		}
		files[fmt.Sprintf("rubi-vm-%s-linux-%s-%s.tar.gz", liveMicroVMVersion, arch, part)] = b
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var sums strings.Builder
	for _, n := range names {
		s := sha256.Sum256(files[n])
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(s[:]), n)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(pub)
	keyFile := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUBI_TEST_RELEASE_KEY_FILE", keyFile)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/"+liveMicroVMVersion+"/")
		switch name {
		case "SHA256SUMS":
			_, _ = w.Write([]byte(sums.String()))
		case "SHA256SUMS.sig":
			_, _ = w.Write(ed25519.Sign(priv, []byte(sums.String())))
		default:
			b, ok := files[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(b)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("RUBI_DOWNLOAD_BASE", srv.URL)

	mem := os.Getenv("RUBI_LIVE_VM_MEMORY")
	if mem == "" {
		mem = "256M"
	}
	be := agent.NewMicroVMBackend(agent.BackendConfig{EnvDir: h.Dir, Limits: agent.VMLimits{Memory: mem}}, liveMicroVMVersion, bin)
	if av := be.Available(context.Background()); !av.OK {
		t.Fatalf("the microvm backend isn't available here: %s", av.Reason)
	}
	h.backendOverride = func() agent.VMBackend { return be }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	start := time.Now()
	if err := h.Setup.Run(ctx, setup.StepContainer, "", "", ""); err != nil {
		t.Fatal(err)
	}
	for h.Setup.Busy() && ctx.Err() == nil {
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("the VM files installed through the setup wizard in %s", time.Since(start).Round(100*time.Millisecond))
	// on the test machine /dev/kvm's access list can be reset after a VM run and is put back by a watcher within about
	// 30 s: wait for the KVM step as the launcher's live tests wait for /dev/kvm
	liveWaitKVM(t, h)
	for _, st := range h.Setup.Status(ctx).Steps {
		switch st.ID {
		case setup.StepContainer, setup.StepService, setup.StepImage:
			if st.State != setup.Done {
				t.Fatalf("setup step %s: %s (%s)", st.ID, st.State, st.Reason)
			}
		}
	}
	vmDir := filepath.Join(h.Dir, "vm")
	t.Cleanup(func() {
		if left := liveProcessesNaming(vmDir); len(left) > 0 {
			t.Errorf("processes of the run are left: %v", left)
		}
	})
	return func(p agent.Project) agent.VM {
		liveWaitKVM(t, h)
		return be.New(p)
	}
}

// liveWaitKVM waits up to 60 s for the setup's KVM step to be done.
func liveWaitKVM(t *testing.T, h *Helper) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		for _, st := range h.Setup.Status(context.Background()).Steps {
			if st.ID == setup.StepService && st.State == setup.Done {
				return
			}
		}
	}
	t.Log("KVM still not usable after 60 s")
}

// liveProcessesNaming lists the processes whose command line names dir (QEMU and virtiofsd name their VM's folder).
func liveProcessesNaming(dir string) []string {
	var out []string
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		if e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err == nil && strings.Contains(string(b), dir) {
			out = append(out, e.Name()+": "+strings.ReplaceAll(string(b), "\x00", " "))
		}
	}
	return out
}
