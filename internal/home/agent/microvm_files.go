//go:build unix

package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/microvm"
	"github.com/Deikus-LXXVII/rubi/internal/integrity"
)

// microVMFiles are the Linux backend's files of one release for this computer's CPU (docs/design/linux-microvm.md, "The
// workflow"): the guest kernel, the root file system with the runner image's environment, QEMU and virtiofsd. They come
// from the release Rubi Home itself is (Version), checked against its signed SHA256SUMS, and live in Root/<version>-<arch>.
type microVMFiles struct {
	Root    string
	Version string
	Arch    microvm.Arch
}

// The archives of a release, per CPU (rubi-vm-<version>-linux-<arch>-<part>.tar.gz).
var microVMParts = []string{"kernel", "rootfs", "qemu", "virtiofsd"}

// microVMArchiveMax bounds one archive's download and what it unpacks to (the root file system is the largest).
var microVMArchiveMax int64 = 4 << 30

func (f microVMFiles) dir() string { return filepath.Join(f.Root, f.Version+"-"+string(f.Arch)) }

func (f microVMFiles) archive(part string) string {
	return "rubi-vm-" + f.Version + "-linux-" + string(f.Arch) + "-" + part + ".tar.gz"
}

// The installed files' places.
func (f microVMFiles) Kernel() string {
	if f.Arch == microvm.AArch64 {
		return filepath.Join(f.dir(), "kernel", "Image-aarch64")
	}
	return filepath.Join(f.dir(), "kernel", "vmlinux-x86_64")
}
func (f microVMFiles) Rootfs() string   { return filepath.Join(f.dir(), "rootfs", "rootfs.img") }
func (f microVMFiles) ImageEnv() string { return filepath.Join(f.dir(), "rootfs", "rootfs.env") }
func (f microVMFiles) qemuDir() string  { return filepath.Join(f.dir(), "qemu", "qemu-"+string(f.Arch)) }
func (f microVMFiles) QEMU() string {
	return filepath.Join(f.qemuDir(), "bin", "qemu-system-"+string(f.Arch))
}
func (f microVMFiles) Firmware() string {
	if f.Arch == microvm.AArch64 {
		return ""
	}
	return filepath.Join(f.qemuDir(), "share", "qemu", "qboot.rom")
}
func (f microVMFiles) virtiofsdDir() string {
	return filepath.Join(f.dir(), "virtiofsd", "virtiofsd-"+string(f.Arch))
}
func (f microVMFiles) Virtiofsd() string { return filepath.Join(f.virtiofsdDir(), "bin", "virtiofsd") }

// Libs are the folders virtiofsd's libraries are in: its own, then the system's (glibc and its loader).
func (f microVMFiles) Libs() []string {
	libs := []string{filepath.Join(f.virtiofsdDir(), "lib")}
	for _, l := range []string{"/lib", "/lib64", "/usr/lib", "/usr/lib64"} {
		if fi, err := os.Stat(l); err == nil && fi.IsDir() {
			libs = append(libs, l)
		}
	}
	return libs
}

// complete marks a finished install (written last, so a half-done one is never taken for installed).
const microVMComplete = "complete"

// Present says whether this release's files are installed.
func (f microVMFiles) Present() bool {
	_, err := os.Stat(filepath.Join(f.dir(), microVMComplete))
	return err == nil
}

// LoadImageEnv reads the runner image's environment (one NAME=value per line).
func (f microVMFiles) LoadImageEnv() (map[string]string, error) {
	b, err := os.ReadFile(f.ImageEnv())
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok && k != "" {
			env[k] = v
		}
	}
	return env, nil
}

// Install downloads this release's archives, checks each against the release's signed SHA256SUMS (signed with the
// release key), unpacks them into a folder of their own and only then puts it in place. Nothing unchecked is unpacked.
func (f microVMFiles) Install(ctx context.Context, log func(string)) error {
	if f.Version == "" || strings.ContainsAny(f.Version, "/\\") || f.Version == "." || f.Version == ".." {
		return errors.New("this Rubi Home has no release version to download the VM files of")
	}
	base := integrity.ReleaseBase(f.Version) + "/" + f.Version + "/"
	sums, err1 := integrity.Fetch(ctx, base+"SHA256SUMS")
	sig, err2 := integrity.Fetch(ctx, base+"SHA256SUMS.sig")
	if err := errors.Join(err1, err2); err != nil {
		return fmt.Errorf("the release's checksums: %w", err)
	}
	if err := integrity.Verify(integrity.TrustedKey(f.Version), sums, sig); err != nil {
		return errors.New("the release's checksums are not signed with the Rubi release key")
	}
	if err := os.MkdirAll(f.Root, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(f.Root, ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, part := range microVMParts {
		name := f.archive(part)
		want, ok := integrity.Lookup(sums, name)
		if !ok {
			return fmt.Errorf("the release has no %s", name)
		}
		log("Downloading " + name + "...")
		file := filepath.Join(tmp, name)
		if err := download(ctx, base+name, file, want); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := unpackChecked(file, filepath.Join(tmp, part)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		_ = os.Remove(file)
	}
	if err := os.WriteFile(filepath.Join(tmp, microVMComplete), []byte(f.Version+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.RemoveAll(f.dir()); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.dir()); err != nil {
		return err
	}
	log("The VM files of " + f.Version + " are installed.")
	return nil
}

// download writes url to file and checks its SHA-256; it stops past microVMArchiveMax.
func download(ctx context.Context, url, file, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := integrity.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	out, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, microVMArchiveMax+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > microVMArchiveMax {
		return errors.New("larger than any VM file")
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return errors.New("its checksum isn't the signed one")
	}
	return nil
}

// unpackChecked unpacks a checked archive into dest: folders and regular files only (no links, devices or anything that
// leaves dest), modes reduced to 0755 or 0644, the total bounded.
func unpackChecked(file, dest string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dest, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(h.Name, "\\") {
			return fmt.Errorf("%q leaves its folder", h.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if h.Size < 0 || total > microVMArchiveMax {
				return errors.New("unpacks larger than any VM file")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, err = io.CopyN(out, tr, h.Size)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("%q isn't a plain file or folder", h.Name)
		}
	}
}
