#!/bin/sh
# build-kernel.sh <x86_64|aarch64> <linux-<version>.tar.xz> <out dir>: the guest kernel of the Linux VM backend, from
# the pinned source (kernel.pin) and the configuration fragments in kernel/. Writes <out>/vmlinux-x86_64 (an ELF with a
# PVH entry) or <out>/Image-aarch64, and the configuration it was built with. CROSS_COMPILE is passed to make.
set -eu
arch=$1 tarball=$2 out=$3
here=$(cd "$(dirname "$0")" && pwd)
pin=$(grep -v '^#' "$here/kernel.pin")
ver=${pin%% *} sum=${pin##* }
echo "$sum  $tarball" | sha256sum -c -
case $arch in
x86_64) karch=x86 target=vmlinux image=vmlinux ;;
aarch64) karch=arm64 target=Image image=arch/arm64/boot/Image ;;
*) echo "unknown CPU $arch" >&2; exit 2 ;;
esac
mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
tar -xJf "$tarball" -C "$work"
cd "$work/linux-$ver"
export KBUILD_BUILD_USER=rubi KBUILD_BUILD_HOST=rubi KBUILD_BUILD_TIMESTAMP="$(date -u -d @0 2>/dev/null || date -u -r 0)"
make -s ARCH=$karch allnoconfig
scripts/kconfig/merge_config.sh -m -O . .config "$here/kernel/common.config" "$here/kernel/$arch.config" >/dev/null
make -s ARCH=$karch olddefconfig
sh "$here/check-kernel-config.sh" "$arch" .config
cp .config "$work/checked.config"
make -s ARCH=$karch -j"$(nproc)" $target
# the configuration the kernel was built with is the one checked (make may rewrite .config)
sh "$here/check-kernel-config.sh" "$arch" .config
cmp -s .config "$work/checked.config" || { echo "the configuration changed during the build" >&2; diff "$work/checked.config" .config >&2; exit 1; }
case $arch in
x86_64) cp vmlinux "$out/vmlinux-x86_64" ;;
aarch64) cp "$image" "$out/Image-aarch64" ;;
esac
cp .config "$out/config-$arch"
echo "built $arch kernel $ver"
