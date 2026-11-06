#!/bin/sh
# build-virtiofsd.sh <virtiofsd-<version>.crate> <out dir>: virtiofsd from the pinned crate (virtiofsd.pin), built with
# its own Cargo.lock (--locked: every dependency's checksum is the lock file's), as <out>/virtiofsd-<arch>/: bin/virtiofsd
# and the libraries it loads that aren't glibc's (lib/, through $ORIGIN/../lib), with the licence texts. The launcher's
# Landlock rule set allows this folder and the system's library folders (Launcher.Libs).
set -eu
crate=$1 out=$2
here=$(cd "$(dirname "$0")" && pwd)
pin=$(grep -v '^#' "$here/virtiofsd.pin")
ver=${pin%% *} sum=${pin##* }
echo "$sum  $crate" | sha256sum -c -
arch=$(uname -m)
mkdir -p "$out"
out=$(cd "$out" && pwd)
dest=$out/virtiofsd-$arch
rm -rf "$dest"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
tar -xzf "$crate" -C "$work"
cd "$work/virtiofsd-$ver"
RUSTFLAGS='-C link-args=-Wl,--disable-new-dtags,-rpath,$ORIGIN/../lib' cargo build --release --locked
mkdir -p "$dest/bin" "$dest/lib"
cp target/release/virtiofsd "$dest/bin/"
strip "$dest/bin/virtiofsd"
ldd "$dest/bin/virtiofsd" | awk '/=> \// { print $3 }' | while read -r lib; do
	case ${lib##*/} in
	libc.so.*|libm.so.*|libpthread.so.*|libdl.so.*|librt.so.*|libgcc_s.so.*|ld-linux*) ;;
	*) cp -L "$lib" "$dest/lib/" ;;
	esac
done
# every library it loads, but glibc's, comes from the folder (the RPATH, unlike a RUNPATH, also serves the
# libraries' own dependencies, such as glib's)
outside=$(ldd "$dest/bin/virtiofsd" | awk '/=> \// { print $3 }' | grep -vE '/(libc|libm|libpthread|libdl|librt|libgcc_s)\.so|/ld-linux' |
	while read -r lib; do case $(readlink -f "$lib") in ("$dest"/lib/*) ;; (*) echo "$lib" ;; esac; done)
if [ -n "$outside" ]; then
	echo "these libraries aren't in the folder: $outside" >&2
	exit 1
fi
cp LICENSE* "$dest/" 2>/dev/null || true
"$dest/bin/virtiofsd" --version
echo "built virtiofsd $ver for $arch: $(du -sh "$dest" | cut -f1)"
