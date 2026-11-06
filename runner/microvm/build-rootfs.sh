#!/bin/sh
# build-rootfs.sh <runner.tar> <rubi-guest> <out.img>: the guest's root file system: the runner image's files (a
# `docker export` / `container export` of it) with the guest agent as /sbin/rubi-guest, as an ext4 image the VM attaches
# read-only. Runs as root (the files keep their owners). Stops when the runner's user and group aren't GuestUID and
# GuestGID (10001): virtiofsd maps exactly those to the user (microvm.VirtiofsdArgs).
set -eu
tarball=$1 guest=$2 out=$3
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
root=$work/root
mkdir "$root"
tar -xf "$tarball" -C "$root" --numeric-owner
ids=$(awk -F: '$1 == "rubi" { print $3 ":" $4 }' "$root/etc/passwd")
group=$(awk -F: '$1 == "rubi" { print $3 }' "$root/etc/group")
if [ "$ids" != "10001:10001" ] || [ "$group" != "10001" ]; then
	echo "the runner's user rubi is $ids (group rubi: $group); the VM needs 10001:10001" >&2
	exit 1
fi
grep -v '^#' "$here/rootfs-dirs.txt" | while read -r d; do
	[ -n "$d" ] && mkdir -p "$root$d"
done
install -m 0755 "$guest" "$root/sbin/rubi-guest"
# what a container runtime fills in at start, empty here
: > "$root/etc/hostname"
# size: the files and a margin; no journal (the VM mounts it read-only), fixed identity, so the same input gives the
# same image
kib=$(du -sk "$root" | cut -f1)
size=$((kib + kib / 5 + 65536))
rm -f "$out"
E2FSPROGS_FAKE_TIME=0 mkfs.ext4 -q -F -L rubi-root -U 2b8f5a0e-6c1d-4a5e-9b7e-7275626900ff -E root_owner=0:0,hash_seed=2b8f5a0e-6c1d-4a5e-9b7e-7275626900ff \
	-O ^has_journal -d "$root" "$out" "${size}k"
echo "built $out ($((size / 1024)) MiB)"
