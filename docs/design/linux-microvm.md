# Agent VMs on Linux: the microvm backend (stage P9 spike)

This note answers the open technical questions of `docs/plans/home-platforms.md` (section 2.7) and fixes the design the
P9 work builds. Where an answer rests on a source, the source is named; where it is still to be measured on a computer
with KVM, it says so. Nothing here has run on a real KVM host yet: there is no test machine with KVM so far, so every
"measure" item is open until there is one.

The code that exists so far is `internal/home/agent/microvm`: the QEMU and virtiofsd command lines, the guest's mount
table and the KVM check, with their tests. It starts nothing yet.

## The shape

One VM per project (or per isolated role), as with Apple's `container` on macOS:

- **QEMU** (`qemu-system-x86_64` or `qemu-system-aarch64`), one process per VM, run as the user. A pinned build that Rubi
  Home downloads and checks against the release signature, like T3 and Node (owner's decision). KVM only, no TCG.
- **virtiofsd**, one process per shared folder, run as the user; the same pinned, signed delivery.
- **A guest agent** as PID 1 (`/sbin/rubi-guest`, a small static Go program in the root file system). It mounts the
  shares, starts processes with stdio and carries the netpipe frames, over one virtio-serial port.
- **Kernel and root file system**, built in CI per CPU from `runner/container/Dockerfile`, signed in the release's
  `SHA256SUMS`, downloaded once per runner hash. The root is a raw ext4 image attached read-only.

## The questions

### 1. Can the user use `/dev/kvm`?

systemd's udev rules give `/dev/kvm` (and `/dev/vhost-vsock`, `/dev/vhost-net`) the `kvm` group and a mode set when
systemd is built (`DEV_KVM_MODE`); the rules carry no `uaccess` tag (systemd, `rules.d/50-udev-default.rules.in`). The
mode varies by distribution, so Rubi Home does not guess: it opens `/dev/kvm` and asks `KVM_GET_API_VERSION`, which
must be 12 (`microvm.CheckKVM`, `HostKVMProbe`). The answers:

- opens and says 12: available;
- not there, and an x86 CPU without the `vmx` or `svm` flag: "agents need hardware virtualisation, which this computer's
  CPU doesn't offer to Linux" (a VPS or VM without nested virtualisation, an old CPU);
- not there otherwise: no KVM (turned off in the firmware, or the module isn't loaded);
- permission denied: the user is not in the `kvm` group. Adding them is a root step; the panel says so in words (project
  rule 2: no terminal commands in user text). Whether Rubi Home offers to do it with the administrator step of 2.4 is
  a later decision; it is not part of P9.

### 2. Which machines get "agents need hardware virtualisation"?

Every computer where the check above fails: most VPSes and cloud VMs (no nested virtualisation), 32-bit computers, and
ARM boards whose kernel has no KVM. The check is the answer, not a list. Raspberry Pi: not confirmed from a source for
the Pi 5 with the stock kernel; to be tried on a real board.

### 3. virtio-fs or 9p?

**virtio-fs.** It is the faster of the two and the one QEMU keeps developing; 9p is not deprecated (QEMU removed only
its `proxy` backend, in 9.2), so it stays the fallback if virtiofsd turns out unusable somewhere.

File-change events: a watcher in the guest does **not** see the user's edits on the computer through virtio-fs (FUSE
has no remote fsnotify; an upstream series to add it was posted in 2021 and not merged). The agent itself does not
watch files; tools that do (dev servers) use polling inside the VM (`CHOKIDAR_USEPOLLING`, `WATCHPACK_POLLING`), which
the runner can set. If that proves a problem, Home can relay the computer's own file events over the control channel.

### 4. Shipping QEMU and virtiofsd

**Pinned builds signed with the release key** (owner's decision), like T3 and Node:

- CI builds QEMU per CPU with only what the VM uses: `--target-list=<arch>-softmmu --without-default-features
  --enable-kvm --enable-seccomp --enable-vhost-user` plus the virtio devices of the command line. `-sandbox` needs the
  seccomp build. The build is relocatable (a folder with its libraries), not a glibc static binary, which QEMU
  discourages. Size: to be measured (expected tens of MB).
- virtiofsd (Rust) is built with the musl target where its libseccomp and libcap-ng allow, else as a relocatable folder.
- Both go into the release's `SHA256SUMS` and are checked exactly as the Home binary is (`docs/design/rubi-home.md`).
- **Licences.** QEMU is GPL-2.0-or-later: the release must carry or offer the exact source and build scripts it was built
  from. The release job attaches a source archive of the pinned QEMU with the build script. virtiofsd is Apache-2.0
  and BSD-3-Clause: the licence texts go with it. Signing does not conflict with GPLv2.

### 5. vsock or serial?

**virtio-serial** for the control channel. It needs no kernel module and no device node on the computer, no CID to
allocate, and nothing but a Unix socket QEMU serves in the VM's folder (`control.sock`, port `rubi.control`). vsock
needs `/dev/vhost-vsock` (the same `kvm` group as `/dev/kvm`) and a host-wide unique CID; the unprivileged alternative,
`vhost-device-vsock`, is one more daemon to ship. Throughput of virtio-serial is not documented; it is expected in the
tens to low hundreds of MB/s, enough for a command's stdio, the proxy and the hook gate. **Measure** with a T3 session
before the backend is called done; vsock stays the option if it is too slow.

### 6. Image size and boot time

Not measured yet. A root file system with Node and the Agent SDK is expected at a few hundred MB per CPU uncompressed;
the download is compressed and happens once per runner change. A trimmed kernel on the `microvm` machine is expected to
reach the guest agent in well under a second. **Measure** both on the first KVM host; the setup wizard shows the
download size before it starts.

### 7. Memory

A VM per role per project, as on macOS. The defaults are 2 CPUs and 2 GiB (`microvm.DefaultCPUs`,
`DefaultMemoryMiB`); the setup wizard's `CPUs` and `Memory` are the limits, accepted only as a count of 1 to 999 and a
size of 256 MiB to 256 GiB (`2G`, `1536M`). A shared VM for a project's roles is not done: it would weaken the role
boundary. The guest's memory is a shared memfd (vhost-user needs it), so it is not swapped by the computer like an
ordinary process's; the wizard says how many VMs the chosen size allows.

### 8. Windows file sharing

Unchanged from the plan: QEMU with WHPX has no virtio-fs or 9p on a Windows host (a 2022 series for 9p with
`security_model=none` was not confirmed merged; virtiofsd does not run on Windows). P12 needs a file service of its own
over the control channel. Not part of P9.

### 9. Same behaviour as Apple's inside `/work`

virtio-fs passes hard links, symlinks, case sensitivity and mode bits through to the computer's file system, so the
shield and snapshot code (`agent/shield.go`) sees what it sees on macOS with bind mounts (and Linux file systems are
case-sensitive, unlike the macOS default). Ownership: the runner (uid 10001, `microvm.GuestUID`) and the user are
mapped onto each other by virtiofsd (`--translate-uid`, `--translate-gid`), so what the agent writes is the user's. To
be confirmed by the shield tests in a live VM.

## Isolation

What keeps an agent in its VM, and what keeps the VM's helpers in their place:

- **No network device.** `-nic none` with `-nodefaults`: the VM has no network card of any kind. The proxy, the hook
  gate and T3's port are netpipe streams over the control channel, as with Apple's `container`.
- **QEMU** runs as the user, under its seccomp sandbox (`-sandbox on,obsolete=deny,elevateprivileges=deny,spawn=deny,
  resourcecontrol=deny`, the set libvirt uses), with no default devices, no user config file, no display, no monitor,
  and `-no-reboot` (a guest reboot or panic ends QEMU). `-run-with user=` and `chroot=` need root and are not used.
- **The root file system** is read-only (`readonly=on` on the drive); the guest's writable places are tmpfs and the
  shares.
- **The shares.** Each folder of `agent.Project.Mounts` is its own virtiofsd and its own tag (`rubi0`...), in the same
  order, and the guest agent mounts them as the table says (read-only ones with `ro`). A read-only share is also refused
  writes by virtiofsd itself (`--readonly`).
- **virtiofsd's confinement.** Its own sandbox (`--sandbox namespace`) needs a user namespace with capabilities, which
  Ubuntu 24.04 does not give unconfined programs (`kernel.apparmor_restrict_unprivileged_userns`), and Rubi Home installs
  nothing as root to change that. So virtiofsd runs with `--sandbox none`, and the launcher confines it instead: before
  starting virtiofsd it applies a **Landlock** rule set (unprivileged, Linux 5.13 and later) that lets the process reach
  only its shared folder, read-only for a read-only share, plus the libraries it loads. On a kernel without Landlock the
  backend is not available (it does not run an unconfined file server). virtiofsd's own seccomp filter stays on.
- **Paths.** Every path given to QEMU must be absolute and clean, with no control characters; a comma inside an option
  value is doubled (QEMU's only escape), so a folder named `a,readonly=off` can't add an option. Guest places must be
  absolute, clean and not `/`, and no place is shared twice.

## The guest agent

`cmd/rubi-guest` is the VM's PID 1 (`init=/sbin/rubi-guest`), a static Go program; its logic is
`internal/home/agent/guest`, which also holds Home's client.

- **Start.** As PID 1 it mounts `/proc`, `/sys` (read-only), `/dev`, `/tmp` and `/run`, starts itself again as
  `rubi-guest serve`, and from then on only reaps: every orphan of the VM becomes PID 1's child, and a wait for any
  child must not take an exit status the server is waiting for, so the server runs in a process of its own. When the
  server ends, PID 1 powers the VM off.
- **The control channel.** The server finds the virtio-serial port by its name (`/sys/class/virtio-ports/*/name` is
  `rubi.control`; there is no udev in the guest to make `/dev/virtio-ports/`), opens `/dev/vportNpM` and serves it with
  package netpipe's frames. It says HELLO when it is ready. Home opens every stream and the guest opens none, so stream
  numbers never collide.
- **Control requests** (service 1): JSON lines, one answer per request: `mount` (the table of
  `microvm.GuestMounts`; each share is mounted `nosuid,nodev`, and `ro` when read-only), `ping`, `poweroff`.
- **Processes** (service 2): one stream per process. Home writes the request (argv, the whole environment, the folder,
  the user and group: never root), the guest answers whether it started, then chunks follow (a kind byte, a 4-byte
  length, at most 32 KiB): stdin and its end from Home; stdout, stderr and, last, the exit status from the guest. A
  stream that Home closes kills the process's whole process group. The process runs as the request's user with no
  extra groups, in a session of its own, with nothing of the guest agent's environment.
- **Trust.** What runs in the VM may take over its root, so the guest is the less trusted side: Home's client bounds
  every line (64 KiB) and chunk (32 KiB) it reads, treats any other kind of chunk as a protocol error, and gives the
  guest only what the request carries.
- **The netpipe** of the proxy, the hook gate and T3's port stays what it is: a process (`rubi-netpipe`) started in the
  VM with this exec, whose stdio carries its own multiplexed streams.

## libkrun

libkrun (Apache-2.0) is a VMM library with virtio-fs, vsock and KVM (and HVF on macOS) built in, used rootless by
crun-krun. It would replace QEMU and virtiofsd with one library, and could one day replace Apple's `container` too. Not
now: its current major version's API is not stable, it is a C library (cgo, or a helper binary), and it turns on its
TSI networking whenever the VM has no network card, which is exactly the case Rubi Home relies on; its file server is
not confined either. It is worth a second look once its API is stable.

## What P9 still has to do

In order; each step has its tests:

1. The guest agent (`/sbin/rubi-guest`): done (above); tested over a pipe, its init only in a live VM.
2. The launcher: Landlock for virtiofsd, the VM folder (0700), starting virtiofsd and QEMU, the pid file, the sweep.
3. CI builds: kernel, root file system, QEMU, virtiofsd per CPU, into `SHA256SUMS`; the QEMU source archive.
4. The backend (`VMBackend` on Linux: `Available` from `CheckKVM`, `New`, `Sweep`, `Provision` with the download steps),
   chosen in `platformBackend` for Linux.
5. T3 sessions: the `agent-vm` shim's stream attached to a guest exec.
6. The live test on a computer with KVM (boot, exec, no network, a read-only share refuses writes, a kill leaves nothing),
   and the measurements of questions 5 and 6.

## Sources

- systemd udev rules: https://github.com/systemd/systemd/blob/main/rules.d/50-udev-default.rules.in
- virtiofsd README (sandbox modes, unprivileged use, `--translate-uid`, `--readonly`): https://gitlab.com/virtio-fs/virtiofsd/-/blob/main/README.md
- Ubuntu 24.04 user namespace restriction: https://seclists.org/oss-sec/2024/q2/147
- FUSE/virtiofs inotify series: https://lore.kernel.org/linux-fsdevel/YXm4eNztvEyGGp4L@redhat.com/
- QEMU removed features (9p proxy, `-chroot`, `-runas`): https://www.qemu.org/docs/master/about/removed-features.html
- QEMU microvm machine: https://www.qemu.org/docs/master/system/i386/microvm.html
- QEMU manual (`-sandbox`, `-nic`, `-fsdev`): https://www.qemu.org/docs/master/system/qemu-manpage.html
- vhost-device-vsock: https://docs.rs/crate/vhost-device-vsock/latest/source/README.md
- libkrun: https://github.com/containers/libkrun
- QEMU WHPX: https://www.qemu.org/docs/master/system/whpx.html
