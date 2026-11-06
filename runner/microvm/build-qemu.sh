#!/bin/sh
# build-qemu.sh <x86_64|aarch64> <qemu-<version>.tar.xz> <out dir>: the QEMU of the Linux VM backend, from the pinned
# source (qemu.pin), as a relocatable folder <out>/qemu-<arch>/: bin/qemu-system-<arch>, the libraries it loads that
# aren't glibc's (lib/, found through $ORIGIN/../lib), and the one firmware file the command line names (qboot.rom on
# x86). Only what microvm.QEMUArgs uses is built: KVM (no emulator), seccomp (-sandbox), vhost-user (virtiofsd), no
# display, no network back ends, no tools. Build on the oldest glibc the release supports (Ubuntu 22.04), with:
# build-essential python3 python3-venv python3-tomli ninja-build pkg-config libglib2.0-dev libseccomp-dev libattr1-dev
# libfdt-dev flex bison bzip2 xz-utils.
set -eu
arch=$1 tarball=$2 out=$3
here=$(cd "$(dirname "$0")" && pwd)
pin=$(grep -v '^#' "$here/qemu.pin")
ver=${pin%% *} sum=${pin##* }
echo "$sum  $tarball" | sha256sum -c -
mkdir -p "$out"
out=$(cd "$out" && pwd)
dest=$out/qemu-$arch
rm -rf "$dest"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
tar -xJf "$tarball" -C "$work"
cd "$work/qemu-$ver"
./configure --target-list="$arch-softmmu" --prefix=/ --without-default-features \
	--enable-kvm --disable-tcg --enable-seccomp --enable-vhost-user --enable-vhost-kernel --enable-attr \
	--disable-docs --disable-tools --disable-guest-agent --disable-user --disable-werror \
	--enable-fdt=system --extra-ldflags='-Wl,--disable-new-dtags,-rpath,$ORIGIN/../lib'
make -j"$(nproc)" "qemu-system-$arch"
mkdir -p "$dest/bin" "$dest/lib" "$dest/share/qemu"
cp "build/qemu-system-$arch" "$dest/bin/"
strip "$dest/bin/qemu-system-$arch"
case $arch in
x86_64) cp pc-bios/qboot.rom "$dest/share/qemu/" ;;
esac
# the libraries it loads, but glibc's own (the system's)
ldd "$dest/bin/qemu-system-$arch" | awk '/=> \// { print $3 }' | while read -r lib; do
	case ${lib##*/} in
	libc.so.*|libm.so.*|libpthread.so.*|libdl.so.*|librt.so.*|ld-linux*) ;;
	*) cp -L "$lib" "$dest/lib/" ;;
	esac
done
# every library it loads, but glibc's, comes from the folder (the RPATH, unlike a RUNPATH, also serves the
# libraries' own dependencies, such as glib's)
outside=$(ldd "$dest/bin/qemu-system-$arch" | awk '/=> \// { print $3 }' | grep -vE '/(libc|libm|libpthread|libdl|librt|libgcc_s)\.so|/ld-linux' |
	while read -r lib; do case $(readlink -f "$lib") in ("$dest"/lib/*) ;; (*) echo "$lib" ;; esac; done)
if [ -n "$outside" ]; then
	echo "these libraries aren't in the folder: $outside" >&2
	exit 1
fi
cp COPYING LICENSE "$dest/" 2>/dev/null || true
"$dest/bin/qemu-system-$arch" -version
echo "built QEMU $ver for $arch: $(du -sh "$dest" | cut -f1)"
