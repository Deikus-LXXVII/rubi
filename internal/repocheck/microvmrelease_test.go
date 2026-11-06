package repocheck

import (
	"strings"
	"testing"
)

// The release carries the Linux VM backend's files under the names Rubi Home's setup downloads, in the signed
// SHA256SUMS it checks them against: rubi-vm-<tag>-linux-<x86_64|aarch64>-<kernel|rootfs|qemu|virtiofsd>.tar.gz.
func TestReleaseCarriesTheVMFiles(t *testing.T) {
	rel := readRepo(t, ".github/workflows/release.yml")
	job := strings.Index(rel, "\n  microvm:\n")
	r := strings.Index(rel, "\n  release:\n")
	if job < 0 || r < job {
		t.Fatal("release.yml needs a microvm job before the release job")
	}
	call := rel[job:r]
	for _, want := range []string{"uses: ./.github/workflows/microvm.yml", "version: ${{ github.ref_name }}"} {
		if !strings.Contains(call, want) {
			t.Errorf("the microvm job lacks %q", want)
		}
	}
	body := rel[r:]
	if !strings.Contains(body[:200], "microvm]") {
		t.Error("the release does not wait for the microvm job")
	}
	dl, sums := strings.Index(body, "pattern: microvm-*"), strings.Index(body, "sha256sum rubi-*.tar.gz rubi-*.zip")
	if dl < 0 || sums < dl {
		t.Error("the VM archives are not downloaded into dist before SHA256SUMS lists them")
	}
	if !strings.Contains(body, `ls "dist/rubi-vm-${GITHUB_REF_NAME}-linux-$arch-$part.tar.gz"`) ||
		!strings.Contains(body, "for arch in x86_64 aarch64; do") || !strings.Contains(body, "for part in kernel rootfs qemu virtiofsd; do") {
		t.Error("the release does not check that every VM archive arrived")
	}
	if !strings.Contains(body, "gh release create \"$GITHUB_REF_NAME\" dist/rubi-*.tar.gz") {
		t.Error("the release does not publish the tar.gz archives (the VM files among them)")
	}

	vm := readRepo(t, ".github/workflows/microvm.yml")
	for _, want := range []string{"workflow_call:", "VERSION: ${{ inputs.version", "- arch: x86_64", "- arch: aarch64",
		`dist/rubi-vm-$VERSION-linux-$ARCH-$1.tar.gz`, "tarit kernel ", "tarit rootfs rootfs.img rootfs.env", "tarit qemu ", "tarit virtiofsd ",
		"name: microvm-${{ matrix.arch }}", "name: microvm-sources", `dist/rubi-vm-$VERSION-sources.tar.gz`} {
		if !strings.Contains(vm, want) {
			t.Errorf("microvm.yml lacks %q", want)
		}
	}

	// what the setup downloads: the same names, from the release of Home's own version
	files := readRepo(t, "internal/home/agent/microvm_files.go")
	for _, want := range []string{`"rubi-vm-" + f.Version + "-linux-" + string(f.Arch) + "-" + part + ".tar.gz"`,
		`var microVMParts = []string{"kernel", "rootfs", "qemu", "virtiofsd"}`, `integrity.ReleaseBase(f.Version) + "/" + f.Version + "/"`} {
		if !strings.Contains(files, want) {
			t.Errorf("the setup's download lacks %q", want)
		}
	}
	arch := readRepo(t, "internal/home/agent/microvm/qemu.go")
	for _, want := range []string{`X86_64  Arch = "x86_64"`, `AArch64 Arch = "aarch64"`} {
		if !strings.Contains(arch, want) {
			t.Errorf("microvm's CPU names lack %s", want)
		}
	}
}
