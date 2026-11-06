# Rubi Home on macOS, Linux and Windows (plan)

Rubi Home is built for a Mac. The owner wants it on Linux and Windows too. This plan does not port anything. It
records what ties Home to macOS today (the audit), what replaces each piece on the other two systems, and the stages
in which to do it. `internal/repocheck/homeplatform_test.go` already keeps new code from adding to the list.

Project rule 2 (the user never uses a terminal) holds on all three systems: the Bot installs Home, and the user only
confirms what the system itself asks for.

The owner's decisions on the open questions are in section 5 and are built into the design and the stages. In short:
the first Windows release is Home without agents; agent isolation is a real VM on every OS; the project folder is
chosen in a panel folder browser; Windows verifies releases with an Ed25519 check inside the installer (no OpenSSL, no Git); there is no Authenticode
signing and no Linux package for now.

## 1. Audit

Where it stands today, from a build of `cmd/rubi-home` on each system:

- macOS: builds and is the only fully supported system.
- Linux (amd64): builds. The peer checks, the file lock, the `systemd --user` unit of `panel/install-home.sh` and the
  `non-mac` pairing text exist. Agent VMs, the administrator step's tools and the hook paths are partly in place (see
  below); nothing else was run on a real Linux box for this audit.
- Windows (amd64): does not build. `internal/home/agent` fails (`syscall.Flock`, `syscall.O_NOFOLLOW`,
  `syscall.Stat_t`), and the packages that import it were not reached, so more errors are hidden behind these. Nothing
  in the release builds Home for Windows.

"Separated" below says whether a build tag or a `runtime.GOOS` test already keeps the code from running elsewhere.
"Linux / Windows today" says what is there for the other two systems.

### 1.1 Autostart, the app bundle and the installer (7 items)

| Item | Where | What it does | Separated? | Linux / Windows today |
|---|---|---|---|---|
| LaunchAgent plist | `internal/home/appinstall.go:116-127`, `:271` | Writes `~/Library/LaunchAgents/com.rubi-project.home.plist` | No (the whole file is mac code, no tag) | Linux: only the shell installer writes a systemd unit (`panel/install-home.sh:109-127`). Windows: nothing |
| launchctl | `appinstall.go:33`, `:226`, `:496-520` | `print`, `bootout`, `bootstrap`, `enable`, `kickstart` of `gui/<uid>` | No | as above |
| App bundle detection | `appinstall.go:44`, `:105-110` | `IsAppLaunch`, `InAppBundle`: `*.app/Contents/MacOS`, `Info.plist` bundle id | No; `cmd/rubi-home/main.go:66-69` calls it on every start | Returns false elsewhere by accident (path shape), nothing else |
| App run | `cmd/rubi-home/main.go:66-69`, `:181-215` (`runApp`) | Finder start: install, start at login, show the window | No | none |
| App log and data folder | `appinstall.go:580` (`OpenAppLog`), `:255` | `~/.rubi-home/logs`, `~/.rubi-home` | No, but plain Go; Unix modes only | works on Linux; on Windows `~` is the profile folder, no ACL |
| Mac app build | `scripts/build-home-app.sh:1-91`, `.github/workflows/release.yml:17-48` | `Rubi Home.app` zip, `Info.plist`, ad-hoc `codesign`, `lipo` universal | Yes (own script and macOS runner) | none |
| Shell installer, mac branch | `panel/install-home.sh:85-107` | writes the plist, `launchctl bootout/bootstrap` | Yes (`if [ "$os" = darwin ]`) | Linux branch `:108-128`; Windows: refuses (`:44-47`) |

### 1.2 Dialogs, links and the clipboard (6 items)

| Item | Where | What it does | Separated? | Linux / Windows today |
|---|---|---|---|---|
| `osascript` dialogs | `internal/home/openwindow.go:138-160` (`dialogArgs`, `dialog`) | The pairing window, 3 buttons, `-128` means cancelled | Yes by `env.GOOS != "darwin"` at `:78` | Other systems print the link, the PIN and the code to stdout (`:78-88`); the Bot relays it. Windows: same text, but nothing else builds |
| `open` for the pairing link | `openwindow.go:119` | `/usr/bin/open <link>` | Same switch | none |
| `pbcopy` | `openwindow.go:123` | the code on stdin, never in argv | Same switch | none |
| `open` for the sign-in link | `internal/home/setup/login.go:226` | opens T3's sign-in URL in the browser | No | none |
| Folder chooser | `internal/home/setup/project.go:117` | `osascript` `choose folder` for agent projects | No (guarded by `s.mac()` at `:79`) | "works only on a Mac" error |
| Claude desktop link | `internal/home/startsession.go:82`, `:91` | `/usr/bin/open claude://code/new?...` | Yes by `goos != "darwin"` | error text: "can only open on a Mac" |

### 1.3 The administrator step (4 items)

| Item | Where | What it does | Separated? | Linux / Windows today |
|---|---|---|---|---|
| Hook install via `osascript` | `internal/home/installhook.go:257-277` (`adminPrompt`, `osascriptArgs`, `runOsascript`), `:314` | `do shell script ... with administrator privileges`, `-128` is cancel | No | none; `rubi-home install-hook` run by `sudo` is the maintainer path |
| Tool paths of the root script | `installhook.go:79-88` (`defaultTools`) | mac: `stat -f`, `shasum`, `wheel`, `base64 -D`; else: `stat -c`, `sha256sum`, `root`, `base64 -d` | Yes by `runtime.GOOS == "darwin"` | Linux branch exists; Windows: none (the script is `sh`) |
| Apple `container` installer | `internal/home/setup/container.go:200-260` (`pkgutil`, `spctl`), `:277` (`/private/var/tmp`), `:337-338` (`osascript` admin), `:353`, `:377` | downloads Apple's signed package, checks it, installs as admin | Yes by `s.mac()` (`setup.go:169`) | "works only on a Mac" |
| Plugin install in the same run | `internal/home/installplugin.go:28`, `:137-140` | `/Library/Application Support/Rubi Home/claude-plugin`, `chown -R root:wheel` | No | Linux gets `root` as the group; the path stays the mac one |

### 1.4 Paths (9 items)

| Item | Where | What | Linux / Windows today |
|---|---|---|---|
| Home's folder | `internal/home/home.go:188-192` (`DefaultDir`) | `os.UserConfigDir()/Rubi Home`: `~/Library/Application Support/Rubi Home` | Linux: `~/.config/Rubi Home`; Windows: `%AppData%\Rubi Home` (roaming, wrong for keys) |
| `~/.rubi-home` | `appinstall.go:255`, `:580`; `panel/install-home.sh:13`; `installhook.go:244` (text) | binary copy and logs | same on Linux; Windows: the profile folder |
| Hook binary | `internal/home/hookcheck.go:25` (`DefaultHookBinary`) | `/usr/local/bin/rubi-home`, root-owned | Linux: same path; Windows: none |
| Plugin folder | `installplugin.go:28` | `/Library/Application Support/Rubi Home/claude-plugin` | none |
| Claude desktop sessions | `claude.go:969-975` (`DefaultClaudeDirs`) | `UserConfigDir()/Claude/claude-code-sessions`, `~/.claude/projects` | works through `UserConfigDir` (Windows `%AppData%`); Claude desktop on Linux is not a thing Home can rely on |
| Managed settings of Claude Code | read by the VM only: `t3selftest.go:83` (`/etc/claude-code/managed-settings.json` inside the VM); on the host Home writes none, the docs name them (`docs/design/rubi-home.md:188`, `:482`) | the host file is the one Claude Code reads for managed settings | macOS `/Library/Application Support/ClaudeCode/`; Linux `/etc/claude-code/`; Windows `C:\Program Files\ClaudeCode\`. These are Claude Code's documented places as of writing; check against its docs when P5 starts |
| Deny lists | `startsession.go:158-160`, `agent/manager.go:435`, `setup/project.go:17` | `Library`, `/System`, `/Library`, `/Applications`, `/private/...` | the Linux and Windows equivalents (`/proc`, `/sys` are there; `%AppData%`, `C:\Windows`, `C:\Program Files` are not) are missing |
| Node, T3 download | `internal/home/t3install.go:106-109` (`t3Platform`), `:388`, `:418`, `:446`; `internal/t3bump/bump.go:37`; `t3/upstream.json` | `darwin-arm64`, `linux-arm64`, `linux-x64`; tar.gz archives | Intel Mac and Windows: "T3 has no build for this computer" |
| VM claude binary | `internal/home/agent/t3vm/server.go:53` (`DefaultClaude`) | the `linux-arm64` build of the Agent SDK's claude inside the image | Matches an arm64 VM only; an x64 host runs an x64 VM |

### 1.5 Local sockets, peer credentials, locks and Unix syscalls (14 items)

| Item | Where | What | Separated? | Linux / Windows today |
|---|---|---|---|---|
| Peer PID, mac | `internal/home/peer_darwin.go:15-40` | `LOCAL_PEERPID`, `SysctlKinfoProc` | `_darwin.go` | `peer_linux.go` (`SO_PEERCRED`, `/proc`); `peer_other.go` (`!darwin && !linux`) refuses everything: the hook and sister tools fail closed |
| Peer exe, mac | `internal/home/peerexe_darwin.go:13` | `sysctl kern.procargs2` | `_darwin.go` | `peerexe_linux.go`, `peerexe_other.go` (refuses) |
| Peer of the VM server | `agent/t3vm/peer_darwin.go:25` (`Xucred`), `procs_darwin.go:24-32` | `LOCAL_PEERCRED`, `LOCAL_PEERPID`, `SysctlKinfoProc` | `_darwin.go` | `_linux.go` (`SO_PEERCRED`), `_other.go` refuses |
| File lock | `internal/home/lock_unix.go:18-28`; `agent/manager.go:324-329`; `agent/t3vm/sweep.go:29-33` | `flock` | `lock_unix.go`/`lock_other.go` only for the first; the other two use `syscall.Flock` untagged | Windows: the first is a no-op (the mutex orders one helper), the other two do not compile |
| `O_NOFOLLOW`, `Stat_t`, `Nlink` | `agent/manager.go:324`, `:2518`; `agent/shield.go:198`, `:207`, `:278`; `agent/t3vm/sweep.go:29` | refuse symlinks, count hard links | No | Windows: does not compile |
| Hook owner check | `hookcheck.go:137`, `:145`, `:189`; `hookstate.go:161`; `installhook.go:392`; `appinstall.go:326` | `Stat_t.Uid == 0` and mode bits to decide "root-owned" | No | works on Linux; Windows: does not compile and has no uid |
| Hook run with a process group | `hookcheck.go:72-74` | `Setpgid`, `Kill(-pid)` to stop a hung `version` | No | Windows: does not compile |
| exec replaces the process | `agent/t3vm/client_unix.go:19` | `syscall.Exec` for the `agent-vm` shim | `//go:build unix` | `client_other.go` (`!unix`) returns an error: the shim cannot work on Windows |
| Unix sockets | `claude.go:631`, `:896`; `localsend.go:331`; `wake.go:337`; `claudemcp.go:123`; `agent/t3vm/server.go:210`; `appinstall.go:568` | Home, the hook, the MCP shim and the agent-vm shim talk over `net.Listen("unix")` with mode 0600 | No | Linux: works. Windows: Go supports AF_UNIX on Windows 10 and later, but Unix modes do nothing and there is no peer check |
| Signals | `cmd/rubi-home/main.go:89`, `:255` | `SIGTERM` with `os.Interrupt` | portable (`syscall.SIGTERM` exists on Windows) | works |
| Hue | `internal/home/hue.go` | HTTP and UDP to the bridge | portable | works everywhere |
| Wake | `internal/home/wake.go` | in-process, over the socket | portable apart from the socket | works on Linux |
| Idle-session waiter | `cmd/rubi-home/main.go:70-74` | plain Go | portable | works on Linux |
| homeproto | `internal/homeproto` | wire format and pairing code | portable | works everywhere |

### 1.6 Shortcuts and Apple Home (4 items)

| Item | Where | What | Separated? | Linux / Windows today |
|---|---|---|---|---|
| `shortcuts` CLI | `internal/home/shortcuts.go:26` | `shortcuts list`, `shortcuts run` in the user's "Rubi" folder | No | the command is missing: error "couldn't list the shortcuts ... (create it in the Shortcuts app)" |
| Op names | `internal/home/home.go:549-551` (`shortcuts.list`, `shortcuts.run`) | | No | the same error |
| Capability list | `home.go:707-708` (`can`) | always lists `shortcuts` | No | a Bot is told Shortcuts works on a Linux box |
| Shortcuts folder setting | `home.go:50` (`ShortcutsFolder`), `cmd/rubi-home/main.go:160-161` (`folder`), `:286` | the folder name in the config and a CLI command | No | meaningless outside macOS |

Apple Home reaches Rubi only through Shortcuts (skill text: `skills/grok-bot/rubi-project/SKILL.md:186-189`).
Philips Hue does not depend on macOS.

### 1.7 Agent VMs: Apple's `container` (11 items)

| Item | Where | What | Separated? | Linux / Windows today |
|---|---|---|---|---|
| The VM interface | `internal/home/agent/vm.go:135-143` | `Start`, `Exec`, `Stop`, `State`: small and OS-neutral | n/a | the seam to build on |
| The Apple backend | `agent/apple.go:55-250` | `AppleContainer`: `run -d --name --init --network none --user --cap-drop ALL --read-only --tmpfs --mount type=bind,...`, `exec -i`, `build --progress plain`, `image inspect` | No (no tag) | none |
| CLI location | `apple.go:82-89` | `/usr/local/bin/container`, else `PATH` | No | none |
| Backend chosen in code | `internal/home/home.go:269-280` (`newAgent`); `agentvm.go`; `setup/agents.go`; `setup/probe.go:283`; `t3login.go` | `&agent.AppleContainer{...}` | No | none |
| The tool, as a word | `agent/t3vm/spec.go:88`, `:263`; `agent/t3vm/client.go:128`; `setup/exec.go:29`; `setup/probe.go:208`, `:248`, `:288`; `setup/jobs.go:19`, `:62` | `container ...` argv, `Base(argv[0]) == "container"` | No | none |
| Setup steps | `setup/setup.go:169`, `setup/probe.go:219-220`, `setup/container.go:353` | "works only on a Mac" | by `s.mac()` | the user sees a failed step, not "not available" |
| Service start | `setup/jobs.go:19` | `container system start --enable-kernel-install` | No | none |
| Runner image build | `setup/jobs.go:55-65`, `agent/apple.go:150-170` | `container build` from `runner/container/Dockerfile` | No | the Dockerfile itself is plain Linux |
| No-network pipe | `agent/pipe.go`, `agent/netpipe/` | the VM has `--network none`; `container exec -i rubi-netpipe` carries the proxy and hook streams over stdio | portable Go (`netpipe`) | works on any backend that has `exec -i` |
| In-VM paths | `agent/pipe.go:21`, `agent/auth.go:91`, `agent/apple.go:62` | `/usr/local/bin/rubi-netpipe`, `/opt/rubi-runner/...` | inside the Linux image | portable |
| Image arch | `agent/t3vm/server.go:53` | arm64 claude | | see 1.4 |

### 1.8 Release and install (6 items)

| Item | Where | What | Linux / Windows today |
|---|---|---|---|
| Release artifacts | `release.yml:72-90`, `:94-97` | `rubi-home-<v>-<os>-<arch>.tar.gz` for darwin amd64/arm64 and linux amd64/arm64; `rubi-home-<v>-macos-universal.zip` (the app); SHA256SUMS signed with Ed25519 | no Windows artifact; no Linux package |
| Shell installer | `panel/install-home.sh:27-31`, `:44-47`, `:63-76` | needs `curl`, `tar`, `openssl` (Ed25519 verify), `sha256sum` or `shasum` | Linux: works. Windows: refuses (`uname` has no Windows) and has no `openssl` |
| Hook warning | `install-home.sh:134-160` | `/usr/local/bin/rubi-home`, text names "macOS asks for your administrator password" | wrong text on Linux |
| Panel texts | `panel/js/app.js:1999-2040` (`homeInstallSteps`) | "Download Rubi Home for Mac", the Mac steps, Linux "Bot way only" | no Windows text |
| Bot instructions | `https://rubi-panel.com/install.md` (`skills/grok-bot/rubi-project/SKILL.md`); the panel names a section "Install Rubi Home on the user's computer" (`app.js:1979-1981`) | | Linux: only the shell line (allow-list T-01L in `usertext_allow.txt`) |
| Pins | `t3/upstream.json`, `internal/t3bump` | `darwin-arm64`, `linux-arm64`, `linux-x64` | see 1.4 |

### 1.9 Counts

| Area | Items |
|---|---|
| 1.1 Autostart, app bundle, installer | 7 |
| 1.2 Dialogs, links, clipboard | 6 |
| 1.3 Administrator step | 4 |
| 1.4 Paths | 9 |
| 1.5 Sockets, peers, locks, Unix syscalls | 14 (3 of them macOS-only and already split; 5 Unix-only and not split) |
| 1.6 Shortcuts, Apple Home | 4 |
| 1.7 Agent VMs | 11 |
| 1.8 Release, install | 6 |

There is no Keychain use: Home keeps its keys in files under its folder (mode 0600). `Keychain` and `security` appear
nowhere in `cmd/rubi-home`, `internal/home` or `internal/homeproto`.

The guard test finds 36 (file, item) pairs of these with a literal, listed in
`internal/repocheck/testdata/homeplatform_allow.txt`. The rest (the Unix-only syscalls, the bare `container`
words in struct fields) is found by the Windows cross-build of stage P1.

## 2. Design

One rule for every concern below: a small interface in OS-neutral code, one implementation file per OS with the same
exported and unexported names (`x_darwin.go`, `x_linux.go`, `x_windows.go`, or one `x_other.go` with `!darwin` when two
systems share it). The guard test checks that every `_darwin.go` has the other two. A fake of the interface keeps the
tests free of a real system (as `AppEnv.Run` and `OpenEnv.Run` do now).

### 2.1 Platform paths

- **Interface.** `platformPaths()` returns `{DataDir, StateDir, BinDir, LogDir, HookBinary, PluginDir,
  ManagedSettings}`.
- **macOS (unchanged).** `DataDir ~/Library/Application Support/Rubi Home`, `BinDir ~/.rubi-home/bin`, `HookBinary
  /usr/local/bin/rubi-home`, `PluginDir /Library/Application Support/Rubi Home/claude-plugin`, `ManagedSettings
  /Library/Application Support/ClaudeCode/managed-settings.json`.
- **Linux (XDG).**
  - `DataDir` stays `$XDG_CONFIG_HOME/Rubi Home` (`~/.config/Rubi Home`): it is where a Linux Home already puts its
    files, so no migration.
  - `LogDir $XDG_STATE_HOME/rubi-home` (`~/.local/state/rubi-home`) for new installs, with the current
    `~/.rubi-home/logs` read as a fallback.
  - `BinDir ~/.local/bin` or `~/.rubi-home/bin` (the installer's choice today; keep it).
  - `HookBinary /usr/local/bin/rubi-home`; `PluginDir /usr/local/share/rubi-home/claude-plugin`; `ManagedSettings
    /etc/claude-code/managed-settings.json`.
- **Windows.**
  - `DataDir %LOCALAPPDATA%\Rubi Home` (not `%AppData%`, which roams and would copy keys to other machines).
  - `BinDir %LOCALAPPDATA%\Rubi Home\bin`, `LogDir %LOCALAPPDATA%\Rubi Home\logs`.
  - `HookBinary C:\Program Files\Rubi Home\rubi-home.exe` (admin-writable only), `PluginDir C:\Program Files\Rubi
    Home\claude-plugin`, `ManagedSettings C:\Program Files\ClaudeCode\managed-settings.json`.
- **Deny lists** (`sensitiveHomeDirs`, `sensitiveSysDirs`, `systemTrees`, `deniedTrees`) become per-OS lists next to
  the paths, with `%AppData%`, `%LocalAppData%`, `C:\Windows`, `C:\Program Files*`, `C:\ProgramData` on Windows and
  `/proc`, `/sys`, `/run`, `~/.local`, `~/.cache` on Linux. Path compare on Windows is case-insensitive and refuses
  8.3 short names and reparse points the way the code refuses symlinks.
- **Hook owner check** (`hookcheck.go`, `hookstate.go`): "root-owned and not writable by others" becomes `isTrustedFile`:
  Unix: uid 0 and mode `0o022` clear; Windows: the owner is `Administrators`, `SYSTEM` or `TrustedInstaller` and the DACL
  gives write to no one else (`GetNamedSecurityInfo`).

### 2.2 Opener, clipboard and dialogs

- **Interfaces.** `Opener.Open(url)`, `Clipboard.Copy(text)` (text on stdin or by API, never in argv), `Dialogs.Message(text,
  buttons...) (button)`. There is no native folder chooser (see below). `OpenEnv` and `Setup` take them; the default is the one for
  `runtime.GOOS`.
- **macOS (unchanged).** `open`, `pbcopy`, `osascript` `display dialog`. The `choose folder` dialog goes (below).
- **Linux.**
  - Open: `xdg-open <url>`. It needs a desktop session (`DISPLAY` or `WAYLAND_DISPLAY`); without one, return
    "no display" and fall back to text.
  - Clipboard: `wl-copy` on Wayland, `xclip -selection clipboard` or `xsel -ib` on X11, code on stdin. None found: no
    copy button; the code is shown instead.
  - Dialog: `zenity` or `kdialog` when present (text, up to three buttons). Otherwise the text mode of
    today (`rubi-home open` prints the link, the PIN and the code, and the Bot relays them).
  - A desktop-portal call (`org.freedesktop.portal.FileChooser`) would also serve Flatpak, but needs a D-Bus library;
    leave for later.
- **Windows.**
  - Open: `ShellExecute` through `golang.org/x/sys/windows` (the `start` builtin of `cmd.exe` re-parses `&` in a pairing
    link, so avoid `cmd /c start`; if a command is wanted, `rundll32 url.dll,FileProtocolHandler <url>`).
  - Clipboard: `clip.exe` (system tool, code on stdin; ASCII codes only) or the clipboard API.
  - Dialog: `MessageBox` has fixed button names and at most the standard sets; the pairing window needs custom
    buttons (Close, Copy the code, Open the Rubi panel), so use `TaskDialogIndirect` (needs a common-controls v6
    manifest in the exe). A PowerShell WinForms form is the fallback that needs no manifest but starts a second
    program.
- **Folder chooser: a folder browser in the panel (decision 5).** No native chooser on any OS (it replaces macOS's
  `choose folder`, `setup/project.go:117`). It works the same on a phone and on a box with no display.
  - **Home op `fs.list {path}`** returns the sub-folders of a folder (names, whether each can be chosen, and a parent
    link), never files and never their contents. It starts at the user's home folder and on Windows lists the drives.
    It refuses the deny lists of 2.1 (it does not show that they exist) and does not follow links out of the tree it
    was asked about.
  - **Core op `device.fs.list {device, path}`**, called by the panel only: a user action in a signed-in panel session.
    Bots do not get it, so a Bot can not learn the user's folder names through it.
  - **Choosing** a folder sends the existing `projects.add {path}` path, which still runs `refuseFolder` and writes the
    result to the config. The folder browser shows what it refused and why.
  - **Panel.** A page section with a breadcrumb, a list of folders, and a Choose button, large enough for a thumb
    (project rule 1). It has a panel flow test and a mobile layout check.
- **Text mode is the floor.** If no dialog can be shown (no display, SSH-only box, service session), Home prints the
  link, PIN and code, which is today's non-mac behavior. The Bot relays them.

### 2.3 Autostart (LaunchAgent counterpart)

- **Interface.** `Autostart{Install(bin string) (changed bool), Status() (loaded bool), Restart(), Remove()}` and `AppEnv.Autostart`.
  The launchd code of `appinstall.go` becomes `autostart_darwin.go`; the app flow (`RunApp`) is OS-neutral and calls
  it. The shell installer calls `rubi-home install-autostart` (a new internal subcommand that the Bot, not the user,
  runs) so the unit text lives in Go once.
- **Linux: `systemd --user`.**
  - Unit `~/.config/systemd/user/rubi-home.service` (as in `install-home.sh:109-127`): `ExecStart=<bin> run`,
    `Restart=always`, `WantedBy=default.target`, then `systemctl --user daemon-reload` and `enable --now`.
  - Needs a user manager. On a headless box that nobody logs in to, `loginctl enable-linger <user>` keeps it running
    without a login; whether an unprivileged user may do that for themself depends on polkit (check on Debian,
    Ubuntu, Fedora, Raspberry Pi OS).
  - No systemd (Alpine, containers): XDG autostart `.desktop` file (desktop sessions only); otherwise report
    "Rubi Home can't start at login on this computer" in the panel.
- **Windows: per-user Scheduled Task, or the Run key.**
  - A service runs in session 0 with no desktop and no access to the user's Claude sessions and files: wrong.
  - A Scheduled Task with a logon trigger for the current user, limited run level, restart on failure, no time limit.
    Created from an XML definition (`schtasks /Create /XML`). Check that a standard user can create it without
    elevation; if not, use `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` (no restart on crash, so Home
    supervises itself) .
  - The app log (`rubi-home-app.log`) moves to `LogDir`.

### 2.4 The administrator step (hook and plugin)

The root script becomes one that runs on a given OS under a given elevator, and the hash the user approved binds the
content (this part of `installhook.go` stays as it is).

- **Interface.** `Elevator.RunScript(ctx, plan InstallPlan, prompt string) error` returning `ErrAdminCancelled` when the
  user declined. `InstallPlan` keeps the content inline and hashed; only the carrier changes.
- **macOS (unchanged).** `osascript` `do shell script ... with administrator privileges`.
- **Linux: `pkexec`.**
  - `pkexec /bin/sh -c <script>` (pkexec needs absolute paths and resets the environment, which `privilegedEnv`
    already wants). polkit asks the user's session agent for the password. Exit 126 means "not authorized" and 127
    "dismissed": map both to `ErrAdminCancelled`.
  - The dialog text is polkit's generic text ("Authentication is required to run /bin/sh as root") unless a policy
    file with `org.freedesktop.policykit.exec.*` annotations exists, which would itself need root to install.
    Accept the generic text, and make the Rubi panel say what the dialog is for.
  - No polkit agent (a headless box, decision 2): `pkexec` cannot ask. The Bot installs the hook with `sudo`, after the
    user approved in the panel. The approval card shows the sha256 of the install plan (the full script, with the
    binary's hash inside). Home does not run `sudo` itself: its `hook.install` answers `needs_bot_sudo` with the exact
    command and the hash. The Bot runs that command only; the root process rebuilds the plan from the binary it is given
    and refuses unless its hash equals the approved one (the existing `install-hook` re-derives the plan; it gains
    `--expect-sha256`). The skill tells the Bot to run nothing else with `sudo` for this. The consent is the panel
    approval, not a password; say so in the card.
  - The script's tool table already has the `stat -c`, `sha256sum`, `root` row; add a test that runs it under `sh`
    against a temporary root.
- **Windows: UAC.**
  - Home starts `rubi-home.exe install-hook --elevated --plan <file>` with `ShellExecuteEx` and the `runas` verb.
    Windows shows its UAC prompt on the secure desktop. The prompt names the publisher of the signed binary; an
    unsigned one shows "Unknown publisher" in a yellow bar, as there is no Authenticode signing (decision 4). The card in the panel tells the user to expect that
    yellow bar and to choose Yes only for the request it just approved.
  - The root `sh` script becomes Go code in the elevated process (there is no `/bin/sh`, `chown` or `mktemp`): it
    writes the hook to a temp file in `C:\Program Files\Rubi Home`, checks the hash, sets the ACL (inherit from Program
    Files: Administrators and SYSTEM write, Users read and execute), and renames. The plan file is read from a folder
    only the user and admins can write and its hash is passed on the command line, so a swap between approval and run
    is caught (the same property as the base64 literal in the shell script).
  - The result goes back through an exit code and a result file in the same folder (an elevated process's stdout is
    not connected to the caller).
  - Claude Code runs commands for hooks with the Windows shell it uses (Git Bash in the native build): the hook command in
    the managed settings is a quoted Windows path. Check against Claude Code's docs in P5.

### 2.5 Local sockets and peer credentials

- **Interface (exists).** `osProcs{PeerPID(net.Conn), Info(pid) (uid, start, exe, err)}` in `internal/home`, and the
  same in `agent/t3vm`. Add `ListenLocal(path)` and `DialLocal(path)` to hide `net.Listen("unix")`, the mode 0600 and
  the stale-file cleanup.
- **macOS.** `LOCAL_PEERPID` and `sysctl` (exists).
- **Linux.** `SO_PEERCRED` and `/proc/<pid>/exe` (exists in `peer_linux.go`, `peerexe_linux.go`, `t3vm/peer_linux.go`,
  `procs_linux.go`). Needs a test on a real Linux with two users; the existing tests use fakes.
- **Windows: named pipes.**
  - A pipe at `\\.\pipe\rubi-home-<user sid>` with a DACL that gives access to the current user's SID only
    (`CreateNamedPipe` and a security descriptor from `golang.org/x/sys/windows`).
  - The peer is `GetNamedPipeClientProcessId`; the program is `QueryFullProcessImageName` on that pid; the user is the
    token owner of the process (`OpenProcessToken`).
  - AF_UNIX on Windows is the alternative: Go supports it, the pid comes from the `SIO_AF_UNIX_GETPEERPID` ioctl on
    recent builds (check the oldest Windows 10 build Rubi supports), and there is no permission mode, only the
    folder's ACL. Decide in a spike (P6); named pipes are the default choice because the DACL is part of the object.
  - Only `golang.org/x/sys/windows` (decision 8). `go-winio` is added only if the named-pipe security descriptor can not be
    done without it, and only after the owner agrees.
- **Locks.** (Done in P1, `internal/home/fsx`.) `lockFile` and the two `Flock` uses call `fsx.Lock` and `fsx.TryLock`:
  flock on Unix, `LockFileEx` on Windows. `O_NOFOLLOW` opens go through `fsx.OpenNoFollow`: Unix uses the flag; Windows looks at the path first and compares the
  opened file with what it saw (P1); P6 makes it a handle opened with `FILE_FLAG_OPEN_REPARSE_POINT`. The hard-link count (`shield.go:207`) is
  `GetFileInformationByHandle.NumberOfLinks` on Windows.
- **Process control.** (Done in P1 as `fsx.KillTree`: a process group on Unix; on Windows it only kills the process.)
  P6 makes the Windows form a Job Object (kill on close).
- **Shim.** `agent-vm` replaces itself with `container exec` by `syscall.Exec`. With the VM backends of 2.7 it relays
  stdio over a stream to Home instead, which needs no exec on any OS (`client_other.go` marks the gap today).

### 2.6 Features that stay on macOS

- **Shortcuts and Apple Home** need the Shortcuts app and Apple Home: macOS only for now (decision 6). A scripts folder
  of programs the user lists (Rubi runs only those) is the later counterpart for Linux and Windows: stage P13.
- **Signal.** Home computes its features at start: `Features() map[string]Feature{Available bool, Reason string}`
  with `shortcuts`, `apple_home`, `claude-start` (desktop link), `agent` (VM backend present), `t3`. The `hello` reply
  carries `features` next to `can`. `can` lists only what is available.
- **Ops** for an unavailable feature return a typed error `not_available_here` with the text "That isn't available on
  this computer (it needs macOS)." instead of the CLI's message. Core passes it to the Bot unchanged.
- **Bot.** The tool result is a plain sentence, so the Bot says it. The skill (`SKILL.md`) says: when a tool answers "not
  available on this computer", tell the user and do not try again or look for a workaround.
- **Panel.** The Home page shows the feature as a muted line, "Shortcuts: not available on this computer", with the
  reason, instead of a red failure; the Shortcuts folder field is hidden when unavailable. The setup wizard for
  agents shows "Agents need <backend> on this computer" with a button that starts the install for that backend, or "not
  available on this computer" when there is no backend for the OS at all.
- **Claude desktop link** (`claude-start`): macOS and Windows have the Claude desktop app, Linux has none: available
  when the `claude://` handler is registered (Windows: `HKCR\claude`; Linux: `xdg-mime query default
  x-scheme-handler/claude`).
- **Agents** (`agent`, `t3` sessions in VMs): available where a VM backend is: macOS (Apple `container`), Linux with KVM
  (2.7). On Windows they are "not available on this computer" in the first release (decision 1).
- **Hue** works everywhere.

### 2.7 Agent VMs: a real VM on every OS (decision 3)

The `VM` interface (`agent/vm.go:135`) is the seam: `Start`, `Exec`, `Stop`, `State`. What is Apple-specific is the
implementation (`AppleContainer`), its installer and the `container` argv in the setup and shim code. The owner wants a
real VM boundary everywhere, so rootless Podman and other shared-kernel containers are out, and so are Kata and gVisor
(Kata is a VM, but it comes through containerd and a root configuration; the plan does not use it).

- **macOS: stays on Apple `container`.** It runs each container in its own VM through the Virtualization framework, so it
  is a real VM. It moves behind the interface unchanged (`apple_darwin.go`).
- **Linux: a new `microvm` backend on QEMU/KVM** (Firecracker is the alternative; compared below).
- **Windows: a later option** (after the first Windows release): QEMU with the Windows Hypervisor Platform (WHPX)
  accelerator, which keeps the Linux backend's code.

**The interface.** `VMBackend{Name() string; Available(ctx) Step; Install(ctx) job; Image(ctx) Step; New(project) VM}`,
and every other place that writes `container ...` asks the backend. `home.go:newAgent`, `setup`, `t3login` and the
`agent-vm` shim stop naming the tool. The `VM` interface keeps its four methods; the two things it must also carry are
read-only mounts per project (the role shield already has them) and a stdio stream for a second process (`Exec`).

**The microvm backend, as designed.** The guest is a small Linux VM per project (or per role) with no network card at all.
- **Image.** CI builds, per CPU, a kernel and a root file system from `runner/container/Dockerfile` and signs them into
  the release's `SHA256SUMS`; Home downloads the pair that matches the runner's hash (as `DefaultImage` is tagged by
  that hash today) and checks it. There is no image build on the user's machine, so no `container build` equivalent
  is needed.
- **Control channel.** A guest agent (PID 1 helper, a small static Go program in the image) serves `exec` with stdio over
  a virtio-vsock or virtio-serial stream. The same frame format as `netpipe` carries it (OPEN, DATA, CLOSE...), so the
  proxy, the hook gate and T3's port keep working as they do: the guest connects out to 127.0.0.1:3128 and so on and
  Home receives the streams, with no network device.
- **Mounts.** The project at `/work` read-write and the role directories read-only: virtio-fs (needs `virtiofsd`, which
  can run without root in a user namespace) or 9p. Read-only overlays on `/work/.rubi` and `/work/.claude` are
  separate read-only mounts, as in `RunArgs` now.
- **Isolation of QEMU itself.** `-nic none`, no display, `-sandbox on` (seccomp), `-run-with` user and chroot where
  possible; the QEMU process runs as the user, never as root.
- **Lifetime.** One QEMU process per VM, supervised by Home; `Stop` sends a guest shutdown, then kills; the sweep at
  start kills leftovers found by a pid file in the VM's folder.
- **T3 sessions.** The `agent-vm` shim relays its stdio to Home over the socket, and Home attaches the stream to a guest
  `exec`; no `syscall.Exec` and no `container exec` argv from Home (so `t3vm/client.go:128` and `spec.go` change).
- **Arch.** The guest has the host's CPU architecture (`t3vm/server.go:53` picks `linux-arm64` or `linux-x64` from the
  image).

| Backend | System | Boundary | Mounts | Fit |
|---|---|---|---|---|
| Apple `container` | macOS (Apple silicon) | one VM per container | bind mounts | Keep. Real VM. |
| QEMU/KVM, `microvm` or `q35` machine | Linux | VM (KVM) | virtio-fs or 9p | Chosen. Mature, runs as the user, one file system sharing story, seccomp sandbox. |
| Firecracker | Linux with KVM | VM, minimal device model | none: block devices and vsock only | Fast to boot and small, but no shared folders: the project would have to be copied in and out, or served by a file service of our own over vsock. Poor fit for a project folder the user's editor also changes. Jailer needs root. |
| Cloud Hypervisor | Linux with KVM | VM | virtio-fs | Close to Firecracker, with virtio-fs. A possible later alternative to QEMU; fewer distros package it. |
| QEMU with WHPX | Windows 10/11 (also Home edition, once the "Windows Hypervisor Platform" feature is on) | VM | no virtfs or virtio-fs on Windows hosts: needs a file service of our own | Later. Same guest and agent as Linux. |
| Hyper-V (HCS) | Windows Pro and up | VM | SMB or Plan9 share | Not on Home editions; skip. |
| WSL2 | Windows | one shared VM for all distros | `/mnt/c` | Not a boundary per project; the VM is shared with the user's own WSL work. Skip as the isolation. |

**Open technical questions** (answer in a spike, P9, before committing to the design):
1. **Is `/dev/kvm` usable by the user?** On a desktop the seat's `uaccess` rule grants it; on a headless box the user
   needs the `kvm` group, which is a root step (the sudo path of decision 2). Without KVM, QEMU's software emulation is far
   too slow, and the feature must say "not available on this computer".
2. **Nested virtualisation and small boards.** A VPS or a VM without nested KVM, and a Raspberry Pi (arm64 KVM support
   depends on the board and kernel) will often have no KVM. Which machines does Rubi say "agents need hardware
   virtualisation" for?
3. **virtio-fs or 9p?** virtio-fs is fast and POSIX-correct but needs `virtiofsd` (a separate package or binary to ship and
   verify, root-less mode needs user namespaces enabled). 9p is built into QEMU but slow and has permission quirks.
   Does the project mount need file-change events (a watcher in the guest sees the user's edits)?
4. **Shipping `virtiofsd` and QEMU.** The distro package (root, many versions) or a pinned static build downloaded and
   verified by Home (we choose the version, we own the update)? The signing chain for it.
5. **vsock or serial.** vsock needs `vhost-vsock` on the host (a kernel module, root to load) or, in Firecracker's way, a
   Unix socket proxy; virtio-serial works everywhere QEMU does. Is the throughput of serial enough for T3 and the proxy?
6. **Image size and boot time.** The Dockerfile image has Node and the Agent SDK. A kernel plus root file system built
   from it may be several hundred MB per CPU; the first download, the update when the runner changes, and a boot of a few
   seconds per project are acceptable?
7. **Memory.** A VM per role per project multiplies memory. A shared VM for a project's roles would weaken the role
   boundary. What are the limits the setup wizard offers (today `CPUs` and `Memory` in `SessionSpec`)?
8. **Windows file sharing.** Without virtfs, is a file service over the control channel (a FUSE client in the guest and
   a server in Home that refuses the deny lists) acceptable, or is the Windows option dropped?
9. **Same behaviour as Apple's.** Hard links, symlinks, case, permissions and mode bits inside `/work`: the shield and
   snapshot code (`agent/shield.go`) depends on them.

### 2.8 T3 and Node

- `t3Platform()` knows `darwin-arm64`, `linux-arm64`, `linux-x64`. Add `darwin-x64` (Intel Macs, decision 9) in P10, and
  `win32-x64` as soon as upstream publishes it (`t3/upstream.json`, `t3bump.Platforms`); Windows archives are `.zip`,
  not `tar.gz` (`t3install.go:418`, `:446`). If upstream has no `darwin-x64` build, P10 says so and the panel shows "not
  available on this computer" for T3 on Intel Macs.
- Until then a platform with no pin gets the "not available on this computer" signal from 2.6, not the English error
  at `t3install.go:388`.

### 2.9 Installation through the Bot (following install.md)

`https://rubi-panel.com/install.md` is the Bot skill; the panel gives the user the message "Install Rubi Home on this
computer by following https://rubi-panel.com/install.md". Its section "Install Rubi Home on the user's computer"
gets one subsection per OS. What the Bot runs is for the Bot; what the user does is only to confirm.

| | macOS | Linux | Windows |
|---|---|---|---|
| Bot detects | `uname -s` is Darwin | `uname -s` is Linux | `$env:OS` is `Windows_NT` |
| Bot runs | the shell installer (`install-home.sh`) | the shell installer | a PowerShell installer (`install-home.ps1`), run with `-ExecutionPolicy Bypass -File` from a downloaded copy |
| Installer checks | signed `SHA256SUMS`, then the archive hash | the same | the same; the Ed25519 check is done by the script itself (decision 4), no other program; SHA-256 of the archive through .NET |
| Autostart | LaunchAgent | `systemd --user` | Scheduled Task |
| Hook | the panel button, administrator dialog | the panel button, polkit dialog; on a box with no polkit agent, the Bot's `sudo` after the approval (decision 2) | the panel button, UAC prompt ("Unknown publisher") |
| User confirms | nothing at install; the administrator password in the dialog when the hook is installed | the polkit password when the hook is installed | the UAC prompt when the hook is installed |
| Pairing | `rubi-home open` shows the window: PIN and link; Open the Rubi panel | `rubi-home open` shows a zenity window with the PIN when there is a desktop; otherwise prints the link and PIN and the Bot sends them in chat | `rubi-home open` shows a TaskDialog with the PIN and link and the same three buttons |
| Without a Bot | the app zip (exists) | none (allow-list T-01L): tar.gz and the Bot only (decision 7) | none: an unsigned setup program would meet SmartScreen, so Windows is the Bot's way only (see risks) |

`install.md` says what the user will see ("Windows will ask whether to allow this app to make changes: choose Yes"),
and that the Bot does not run `sudo` or open an elevated shell for the hook, except on a Linux box with no polkit agent,
and then only the command Home returned after the user's approval (the consent is the user's own prompt, or that
approval).
The skill text is owned by the work on `skills/`; this plan lists what it must say.

The PIN rule: the PIN the user types in the panel must come from Home's window where there is one. In text mode
(no display) the Bot sees it. Pairing still needs the user's passkey approval in the panel, so a Bot that pairs
itself would stop there.

### 2.10 Release artifacts

- **macOS.** Unchanged: `rubi-home-<v>-darwin-{arm64,amd64}.tar.gz`, `rubi-home-<v>-macos-universal.zip` (the app),
  in the signed `SHA256SUMS`.
- **Linux.** `rubi-home-<v>-linux-{amd64,arm64}.tar.gz` (exists), and the Bot installs it. No `.deb`, `.rpm` or AppImage
  (decision 7). From P9 the release also carries the microvm kernel and root file system per CPU.
- **Windows.** `rubi-home-<v>-windows-{amd64,arm64}.zip` with `rubi-home.exe` (built `CGO_ENABLED=0 GOOS=windows`), in the
  signed `SHA256SUMS`. Not Authenticode-signed (decision 4).
- **Workflow.** `release.yml:83` gains `windows/amd64 windows/arm64` and packs a zip; a Windows runner job tests the
  exe (`rubi-home version`) before the release. The cross-build job `home-cross` (P1, in `ci.yml`) runs on every push.
- **Feed.** `releases/latest.json` (read by `homeMacAppURL`, `app.js`) gains a Windows file name.

## 3. Stages

Each stage ends with `go test ./...` green, the allow-list shorter by the lines the stage removes, and a changelog
line. Stage names only; the order is the dependency order.

### P1 Cross-build baseline (done)

- **Work.**
  - New package `internal/home/fsx` with one signature and a file per system for: an exclusive file lock (`Lock`,
    `TryLock`, `Unlock`), `OpenNoFollow`, `NonBlock`, `OwnerOf`, `LinkCount` and `KillTree`. Windows has no owner
    answer, so every "is this root's" check there fails closed until P2.
  - `lock_unix.go` and `lock_other.go` became one `lock.go`. `agent/manager.go`, `agent/shield.go`, `agent/t3vm/sweep.go`,
    `hookcheck.go`, `hookstate.go`, `installhook.go` and `appinstall.go` call `fsx`. Two Unix-only tests got a `mkfifo`
    helper per system.
  - CI: job `home-cross` (vet and build of Home for linux, darwin and windows, amd64 and arm64) and job `home-windows`
    (the `fsx` tests on a Windows runner).
  - Repocheck: `homecross_test.go` refuses a Unix-only syscall name in a file that builds for Windows, and checks that the
    CI job is there.
- **Scope of the cross-build.** Home and what it imports build for Windows. The rest of the repository (the core) does not:
  `internal/paths`, `internal/kvm`, `internal/mcpproxy` and `internal/plugins` use `Stat_t`, `Setsid`, `Setpgid` and `Kill`, and
  more is hidden behind them. Porting the core to Windows is not part of this plan, so `GOOS=windows go build ./...`
  is out of scope and CI builds `./cmd/rubi-home` only.
- **Done when.** `go vet` (tests included) and `go build` of `cmd/rubi-home` and the Home packages pass for the three systems.
  The stage adds no allow-list line and removes none.
- **Tests.** `internal/home/fsx` tests (lock, no-follow open, link count, owner, process-group kill) on this host and on the
  Windows runner; the repocheck tests above, break-checked.

### P2 Platform paths (done)

- **Work.**
  - New package `internal/home/hostpaths`, one file per system with the same functions: `DataBase`, `HelperHome`,
    `HookBinary`, `PluginDir`, `ManagedSettings`, and the deny lists (`SensitiveHomeDirs`, `SensitiveSysDirs`,
    `ExactOnlyDirs`, `SystemTrees`, `SystemExact`, `DeniedTrees`). macOS values are exactly the old literals; Linux keeps
    the XDG `DataBase` and gets its own lists and `/usr/local/share/rubi-home/claude-plugin`; Windows uses
    `%LOCALAPPDATA%` and `Program Files` from the system's known folders, not from environment variables.
  - The call sites use it: `DefaultDir`, `DefaultHookBinary`, `DefaultClaudePluginDir` (now variables), `appinstall.go`
    and `OpenAppLog`, `startsession.go`, `agent/manager.go`, `setup/project.go`.
  - `fsx.TrustedByAdmin` and `fsx.TrustedAncestor` replace the bare owner checks in `hookcheck.go` and `installhook.go`.
    Unix: owned by root, no group or other write. Windows: owner is Administrators, SYSTEM or TrustedInstaller, there is
    an access list, and no entry that applies to the file gives a write right to anybody else (a whitelist, so the
    user's own account counts as untrusted too). For a folder above the file only the rights to replace what is inside
    count, because the drive root lets Authenticated Users add folders. The rule is `fsx.CheckACL`, plain logic tested
    with constructed entries on every host; the Windows file only reads the descriptor.
- **Not done here (P5/P6):** the plugin's `.mcp.json` still names `/usr/local/bin/rubi-home`; the install script is still
  `sh`; the hook's folder on Windows is not created by anything yet.
- **Done when.** No source outside `hostpaths` names `~/Library`, `/Library` or `/usr/local/bin/rubi-home`; the Linux
  `DataBase` is unchanged; Windows paths resolve in a test on a Windows runner. The five P2 allow-list lines are gone.
- **Tests.** Golden values for macOS; XDG for Linux; Windows paths; the ACL table; owner and mode on Unix; the deny
  lists follow the platform in `home` and `agent`.

### P3 Opener, clipboard, dialogs and the panel folder browser (done)

- **Work.**
  - `Opener`, `Clipboard` and `Dialogs` (`desktop.go`), one form per system, argv only:
    - macOS: `open`, `pbcopy`, and an `osascript` handler that takes the text as an argument (`desktop_darwin.go`).
    - Linux: `xdg-open`; `wl-copy`, `xclip` or `xsel` with the code on stdin; `zenity` (text joined to `--text=`, `--no-markup`,
      an extra button for the middle label) or `kdialog` (`desktop_xdg.go`). It all needs `DISPLAY` or `WAYLAND_DISPLAY`.
    - Windows: `rundll32.exe url.dll,FileProtocolHandler <link>`; `clip.exe` with the code on stdin; a PowerShell window
      whose script is a constant and whose text, title and labels arrive as ASCII-only JSON on stdin (`desktop_winps.go`).
      Windows has no shell in it and never `cmd /c`.
    - Every form refuses a link that is not http, https or claude, or holds a space, a control character, a quote or a
      character the quoting of a command line could treat specially (`checkLink`).
  - `rubi-home open` uses them. A part a computer lacks is nil, and the window offers what remains; with no window at all
    (no display, no zenity or kdialog, a PowerShell that can not show one) it prints the link, the PIN and the code.
  - `setup/login.go` and `startsession.go` open links through the Opener; the Claude desktop link works on macOS and
    Windows only.
  - The native folder chooser is gone: `project.add` with no folder answers "choose the folder in the Rubi panel".
  - The folder browser: Home op `fs.list` (`setup/fslist.go`), core `ListDeviceFolders` behind the panel's
    `device.fs.list` (a full settings link only), and the panel's `askFolder`, which a plugin page opens with an action's
    `folder {arg, device, label}`. The panel sends the chosen path to the plugin's op, which still raises its approval and
    calls `project.add {dir}`, whose `refuseFolder` decides again.
- **The listing.** Folders only (no file, size, date or content), under the home folder or a disk mount point
  (`hostpaths.BrowseRoots`), never above them; not inside a system tree, a private folder of the home or Rubi Home's own
  folder, and those are not listed in their parent either; no dot folders and no names with control characters; a link
  or junction is listed only if it leads to a folder of the same root that is not one of those; at most 500 entries.
  Each folder says whether it can be chosen and why not.
- **Panel-only.** Core refuses `fs.list` from any plugin (`attestHomeCall`), no manifest can declare an `fs`
  capability, and `device.fs.list` answers a full settings link only.
- **Allow-list.** The 5 P3 lines are gone, and the P7 line for the `open` in `startsession.go` went with them
  (the Opener removed it). 25 lines are left: P4 7, P5 4, P7 1, P8 13.
- **Hardening after review.**
  - A core that has been taken over holds the pairing token and could crawl `fs.list`. Home limits it (30 calls a minute,
    300 an hour, two at once), logs one line per call with a count and no name, and shows the last hour's count in `hello`
    (`fs_list_calls`), which the panel shows next to the computer. A key held by the panel and checked by Home is a design
    decision for the owner and is not built.
  - The roots are the home folder and the removable disks this user has mounted (`/Volumes/*`; `/run/media/<user>/*` and
    `/media/<user>/*`; the drive letters that are not the system drive). Not `/mnt` (WSL's `/mnt/c`), not `/srv`. The browser
    goes 8 levels below a root; another user's home (a `Users` or `home` folder on a disk) is neither listed nor entered.
  - A system tree does not apply inside a root that sits in it (a home in `/var/home`, a disk in `/run/media`); the private
    folders of the home stay denied. A disk's root is never a project.
  - The folder is opened, and the opened folder is checked (final path on Windows, `os.SameFile` on Unix) before its
    entries are read; a Windows junction is not listed. Core asks Home to resolve the folder (`project.resolve`) before it
    lets a `project.add` through, so the folder the user approved is the folder Home adds.
  - A computer that does not run agents says so in the browser ("Projects are for agents, which this computer doesn't run
    yet") and `project.add` answers the same, instead of "works only on macOS".
  - The Windows programs are those of the System32 folder by full path; the window's text cannot be a mnemonic; kdialog's
    text cannot be rich text; osascript gets `--` before the text; the pairing code is printed only when there is really no
    desktop, and a window that fails for another reason reports the error and the panel's address, without a code.
- **Not verified on real systems:** the `--` before osascript's arguments (it must not appear in `argv`; a manual look on a Mac), the zenity and kdialog calls on a Linux desktop, and the PowerShell window and `clip.exe`
  on Windows. The argv, the stdin and the answer handling are tested with fake programs; the windows themselves need a
  look on a Linux desktop and on Windows 11.

### P4 Autostart and the app flow (done)

- **Work.**
  - `Autostart` (`autostart.go`): `Install`, `Restart`, `Remove`, `Loaded`, per user and never elevated. One form per system:
    - macOS (`app_darwin.go`, `autostart_darwin.go`): the LaunchAgent and the `launchctl` calls moved out of
      `appinstall.go` unchanged. The app flow's tests (`appinstall_darwin_test.go`) are the old ones, unchanged, and
      `AgentPlist` is still checked against the heredoc in `install-home.sh`.
    - Linux (`autostart_systemd.go`): the systemd user unit. Its text and its calls (`daemon-reload`, `enable --now`,
      `restart`) are what `install-home.sh` did, byte for byte for a plain path; a path with a space is quoted now.
    - Windows (`autostart_schtasks.go`): a Scheduled Task of the user, from an XML file, run at that user's logon with
      the interactive token and the least privilege. `validateTaskXML` refuses a startup trigger, a trigger or principal
      for anyone else, a group, a system account, a password or S4U logon and the highest rights; `schtasks` is run
      by its System32 path with no `/RU`, `/RP` or `/RL`; "access denied" is `ErrNeedsElevation` and nothing else is
      tried. The approach (a standard user may register a task that runs as themselves at their own logon, which is how
      per-user installers do it) is from Task Scheduler's documented behaviour and is not verified here: the CI job
      creates and deletes a real task as an administrator, and a standard account needs a manual try.
  - `IsAppLaunch`, `InAppBundle`, `bundleID` and the plist are in `app_darwin.go`; `app_notmac.go` has the same functions,
    which say no.
  - `RunApp` takes an `Autostart` (the system's by default) and keeps its sentences: a failed write is "could not set itself
    up to start when you log in", any other failure "could not start itself".
  - `rubi-home install-autostart [--bin PATH] [--remove]` (internal) makes the start at login with the same code;
    `install-home.sh` runs it on Linux and no longer holds the unit. The macOS part of the script is untouched.
  - `OpenAppLog` was already under `hostpaths.HelperHome` (P2).
- **Allow-list.** The 7 P4 lines are gone. 18 are left: P5 4, P7 1, P8 13.
- **Not done:** XDG autostart for a Linux box with no systemd; a start at login for Windows when the Task Scheduler refuses a
  standard user (the run key, which restarts nothing); a log file for the Windows task (the task has no console, so
  `rubi-home run` logs nowhere there yet).

### P5 The administrator step (done)

- **Work.**
  - `Elevator` (`elevate.go`): `Describe`, `Sentence`, `Available`, `Run`. One form per system, each with an argument list and no composed text:
    - macOS (`elevate_darwin.go`): osascript, byte for byte as before (`osascriptArgs`, the scrubbed `PATH`, "-128" is cancel).
    - Linux (`elevate_pkexec.go`): `/usr/bin/pkexec /bin/sh -c SCRIPT`, by absolute path, the script one argument (15 KB with the plugin). A session counts as a desktop by `XDG_SESSION_TYPE` (x11, wayland, mir) or a display. There is no reliable way to ask polkit beforehand whether an agent is registered, so that is found by trying: pkexec's own lines (`Error executing command as another user: ...`, which the script's output cannot imitate at the start of a line) say dismissed, not authorized, or no agent, and Home remembers "no agent" for 5 minutes only.
    - Windows (`elevate_uac.go`): UAC for the system's PowerShell (System32). See "What Windows does and does not protect" below.
  - **The hash.** `PlanSHA256` is the SHA-256 of the exact text root would run: for the shell forms `FullScript()`, for Windows the plan document. `hook.plan` returns it, the `sentence` for the person, and whether the plugin comes along; the panel approval shows target, version, plugin yes/no and the hash. Core hands the hash to `hook.install`, which **requires it** (`plan_needed` without) and makes the plan again: another plan is `plan_changed` and nothing runs.
  - **Headless Linux (no polkit agent).** `hook.install` runs nothing and answers `needs_bot_sudo` with `argv` and `plan_sha256`. `argv` is exactly `["/usr/bin/sudo", "--", "/bin/sh", "-c", SCRIPT]`, SCRIPT being the plan's script and the hash the SHA-256 of that string, so the Bot or core can check it. **Rubi Home's own binary is never run as root**: the first design ran `sudo ~/.rubi-home/bin/rubi-home install-hook`, a file the user can swap after the approval, and that root form (`--expect-plan-sha256`) is gone. Core passes on that one shape and nothing else, with no prefix matching (five elements, those programs, the script's hash equal to the approved one). The script reads the binary once, checks its SHA-256 against the plan, uses absolute tools, checks that the folders above its target are root's, replaces only a file called `rubi-home` and a folder called `claude-plugin` that has the marketplace's own manifest (every earlier install does), and takes a lock folder in the checked directory (stale after 15 minutes), so two runs cannot overlap even when a prompt timed out while the script still ran. `install-hook` as root takes `--to` and `--plugin-to` only at the fixed defaults and refuses a flag given twice. What no part of this can do is stop a Bot that runs something else with sudo: the consent is the approval of the hash, as decided.
  - **What Windows does and does not protect.** No signed elevated helper (decided against), so a process of the same user can interfere with the non-elevated side that starts the elevated one: its code, its environment, the command line it passes on, or it can show a look-alike prompt or start its own UAC request for the same `powershell.exe`, which looks the same because UAC names the program, not the script. What is done: the elevated program is the system's, not Rubi Home's exe (which sits in a folder the user can write); the elevated command line is a short constant bootstrap (1.5 KB, margin 500 under a 2,000 character safe limit for ShellExecute) with the hash and path of the install script in it; the install script carries the hash of the plan file and uses the plan only when it matches; the binary is read once and checked before it is written; the result gets Administrators and SYSTEM full control, Users read and execute, nothing inherited, owner Administrators; a mutex stops overlapping runs; a hook that is running cannot be replaced, and after five tries the error says to close Claude Code. The relay gets a minimal environment (the system's folders, `PATH` of System32, the user's own `USERPROFILE`, `APPDATA`, `LOCALAPPDATA`, `TEMP`, `TMP`); values the user keeps in `HKCU\Environment` may still be applied by Windows. The result is then looked at from outside, never believed: the target's hash, its version and `fsx.TrustedByAdmin` on it and on every folder above it (`verifyAdminOnly`). The script has no folder-above check of its own because that check is made from outside, after. **What cannot be stopped:** the plan's hash covers the binary that `hook.plan` reads from the user-writable install folder. A process of the same user that swaps that binary *before* the plan is made makes a plan for its own binary, and the user approves the hash of that plan; the later checks (the approved hash, `plan_changed`, the read-once check in the elevated script, the look from outside) all hold a *later* swap off, but nothing in Rubi Home can tell the swapped binary from the real one at the time of the plan. The defence is that the same-user process already has the user's account (it could do what the hook does to the user in other ways); a signed binary or a root-owned install source would be needed to go further.
  - The plugin's `.mcp.json` names this computer's hook path (`renderPluginFile`): unchanged bytes on macOS and Linux, Program Files on Windows.
  - Apple's container installer (`setup/container.go`): the osascript command moved to `admin_darwin.go`; its tests to `container_darwin_test.go`. It keeps its escaped AppleScript literal (byte-identical, as before).
- **Allow-list.** The 4 P5 lines are gone. 14 are left: P7 1, P8 13.
- **Not verified on real systems:** UAC and the elevated script as a standard user on Windows (the CI job runs the script on an administrator runner, where UAC does not ask), and pkexec with a real polkit agent.

### P6 Local sockets and peers on Windows (done)

- **Choice: AF_UNIX, not named pipes.** Go's `net.Listen("unix")` works on Windows 10 and later with deadlines and a clean `Close`. A named pipe has a stronger object-level access list, but a `net.Conn` with deadlines over overlapped pipe I/O needs go-winio or several hundred lines of our own (not added, as agreed). `fsx.ListenLocal` and `fsx.DialLocalContext` are the one way to make and reach the three sockets (hook, sister tools, agent-vm), on every system.
- **Who may connect (Windows).**
  - The socket's folder gets a protected access list for the current user alone (`UserOnlySDDL`), read back and checked (`CheckUserOnlyACL`).
  - Every accepted connection is checked: the peer's process comes from the ioctl `SIO_AF_UNIX_GETPEERPID`, and its user SID and logon session must be this process's own (`PeerAllowed`). Another user, another session of the same user, or a peer that cannot be identified (an older Windows without the ioctl, a process this one may not open) is closed at once.
  - A client checks the server it dialled the same way. All of this logic is plain code, tested on every host with constructed access lists and SIDs.
- **Peers.** `osProcs` and `peerExe` for Windows in `internal/home`, and `peerPID`, `parentOf` and `peerIsSelf` in `agent/t3vm`, use `fsx.PeerPID` and `fsx.ProcInfo` (creation time, parent from a process snapshot, program name without `.exe`) and `fsx.ProcessImage`. Anything unknown is an error, and an error is a refusal; a pid of 0 or 1 is refused too.
- **Process trees.** `fsx.KillTree` on Windows: `fsx.StartTree`/`fsx.RunTree` put the process in a Job Object (kill on close) right after the start, and cancelling terminates the job; a command started without them falls back to killing the descendants found by parent links. The hook's `version` run uses `RunTree`.
- **Links.** `fsx.OpenNoFollow` on Windows opens with `FILE_FLAG_OPEN_REPARSE_POINT` and refuses what has the reparse attribute, deciding on the opened handle.
- **The agent-vm shim** no longer needs exec where there is none: `handOver` runs the command with the shim's own input, output and exit status (`relayProcess`), holding the connection to Home open until it ends.
- **Review round (P6).**
  - Peers: a peer is the same user, the same terminal session and the same logon (the token's authentication id; an elevated token of the same user is another logon and is refused). The peer's process is opened right after its id is read and everything is read from that handle, and it must have been created before the connection was accepted (`PeerBorn`), so a reused pid is refused.
  - Reused pids in the walk: `killDescendants` reads each process's creation time (`Proc`), takes a child only if it was created at or after its parent and the root (`Descendants`), and compares the time again on the opened process just before `TerminateProcess` (`Reap`, `SameProcess`). All of it is tested on every host with a fake table that includes a reused pid.
  - Job race: `StartTree` creates the process suspended (`CREATE_SUSPENDED`, set through `SysProcAttr`, x/sys only), puts it in the job, then resumes its thread, so nothing it starts is outside the job. Cancelling always runs the guarded walk after terminating the job. `WaitTree` and `ReleaseTree` let go of the job (`RunTree` uses `WaitTree`).
  - Links: `OpenNoFollow` looks for a reparse point before anything is created or truncated (`NoFollowPlan` never picks a truncating disposition; `O_TRUNC` runs through the handle afterwards), shares read and write only, and does not open folders. `privateFolder` works only inside Home's folders (`InsideRoots`: the data folder, `RUBI_HOME_DIR`, the helper folder), opens the folder without following links, refuses a reparse point and sets the access list through that handle. `IsSocket` accepts only the AF_UNIX file (`os.ModeSocket`), not every reparse point.
  - Shim: the command runs in a job tied to the shim (`RunTree`), so killing the shim kills it; the shim catches Ctrl-C while the command runs and keeps the connection until the command ends.
- **Not verified on a real Windows:** the ioctl (older builds lack it and then every connection is refused: that is the fail-closed answer), and whether the folder's access list alone stops another user's `connect`. The per-connection check does not depend on it. The Windows CI job runs the socket, junction, process and job tests on an administrator runner.
- **Not changed:** the core's own sockets (`internal/daemon`, `internal/mcpproxy`) and the Unix-only `agent` package tests.

### P7 Feature availability (done)

- **Features.** `Features()` (`internal/home/features.go`) gives `shortcuts`, `apple_home`, `claude-start`, `agent` and `t3`, each `{available, reason}`. `hello` carries `features` and `can` lists only what is available (on a Mac the list is what it always was). It is worked out from the system and a `Desktop`, and kept for a minute (only the Claude app check can change while Home runs).
  - macOS: everything.
  - Shortcuts and Apple Home: macOS only ("it needs macOS").
  - `claude-start`: macOS; elsewhere only where a program is registered for `claude://` (`Desktop.Scheme`: `xdg-mime query default x-scheme-handler/claude` on Linux, `reg.exe query HKCR\claude /v "URL Protocol"` from System32 on Windows; fixed arguments, the scheme is checked against a pattern). The launcher itself now only opens the link through the `Opener`; whether the app is there is decided by the feature.
  - `agent`, `t3`: where there is a VM backend. Today only macOS (`hasVMBackend`); Linux until P9, Windows in the first release. P8 turns this into the backend's own answer.
- **The error.** Every operation of a family tied to a feature (`opFeatures`: `shortcuts.*`, `claude-start.*`, `claude-agent.*`, `agent-setup.*`, `t3-agent.*`, `t3-setup.*`) is checked in `Handle` before anything runs, and answers `homeproto.CodedError{Code: "not_available_here"}` with "That isn't available on this computer (reason)." The code travels in the reply (`code`); core maps it to `rubiplugin.CodeHomeNotAvailable` (-32013), and a plugin gets the sentence as the message. `shortcuts.go` keeps the portable part; the command is in `shortcuts_darwin.go`, and `shortcuts_notmac.go` answers the same error.
- **Core and panel.** The approval for pairing names the shortcuts only for a computer that has them. A check of a computer returns `features`; the panel draws one muted line per reason ("Shortcuts and Apple Home: not available on this computer (it needs macOS)."), no red, no button. `rubi-home status` prints the same lines and `rubi-home folder` says it isn't available.
- **Bot.** `SKILL.md` tells the Bot to say once that it isn't available on this computer, and not to retry or look for a workaround.
- **Tests.** `features_test.go` (a table per system with fake desktops, the argv of the handler checks, every op of every unavailable feature read from `Handle`'s source, `hello`, the cache), `core/homenotavailable_test.go` (code mapping, approval text, `features` in the check), `repocheck` (panel line is calm, the Bot-facing text). Tests of the operations run as on a Mac (`features_testenv_test.go`). The P7 line is gone from the allow-list.
- **What a plugin needs** (T3 Agents, Shortcuts users): treat `CodeHomeNotAvailable` like "the capability is not there": cache it as a no, stop polling, do not say "no answer, it may have been started", and show the sentence calmly. The T3 Agents plugin's places are listed in the report of this stage.

### P8 VM backend interface (done)

- **The interface** (`internal/home/agent/backend.go`). `VMBackend{Name; Available(ctx) Availability; New(Project) VM; Sweep; Login() LoginHost; Sessions() SessionHost; Provision() Provisioner}`. It carries what the microvm backend of 2.7 needs, and no more:
  - *lifecycle and exec with stdio*: `New` returns a `VM` (`Start`, `Exec`, `Stop`, `State`; `Conn` is a process's stdin and stdout, however it is carried).
  - *shared folders*: `Project.Mounts()`, the one ordered list of host folders a VM shows (project read-write at `/work`, the shield and the role's content read-only, the role's config directory read-write). Apple's `RunArgs` turns it into `--mount type=bind,...`; a microvm turns each into a virtio-fs share.
  - *control channel*: `StartSpec.Pipe` and `netpipe`, unchanged: no network card, the proxy, the hook gate and T3's port are streams over what the backend provides.
  - *resource limits*: `BackendConfig.Limits{CPUs, Memory}`.
  - *image and setup*: `Provisioner{Titles, Tool, Installed, Service, StartService, ServiceDetail, Image, BuildImage, ImageTag}`; the setup wizard keeps its steps and states and asks the backend for how each stands.
  - *availability*: `Available` says whether this computer can use the backend at all and gives the reason (`Availability`); `NoBackend(reason)` is the backend of a computer with none.
- **macOS unchanged.** `AppleBackend` (`apple_backend.go`) wraps `AppleContainer`. Tool name and path (`ContainerBin`, `installedBin`, `ExecToolName`) and the platform choice are in `apple_darwin.go` with `apple_other.go` beside it (Linux and Windows: no tool, backend "none": "agents don't run on Linux yet" / "...Windows yet"); the package check (`pkgutil`, `spctl`) is in `setup/container_darwin.go` with `container_other.go`. The command-line builders stay plain Go, so the Apple argv tests also run on Linux CI. (This differs from the plan's wording, which moved the whole `AppleContainer` to a darwin file: that would have needed about forty stubs for the guard and taken the argv tests off Linux CI. A `home-macos` CI job now runs the darwin-only tests.)
- **Proof it is byte-identical.** Golden tables taken from the code before the change: `agent/testdata/apple_argv.golden.json` (every `container` command line of a VM: run for a project, a role and a shielded role, exec, stop, delete, image, build, ls, inspect, the sign-in, a whole start and stop), `setup/testdata/container_argv.golden.json` (the wizard's probes and jobs) and `agent/t3vm/testdata/session_argv.golden.json` (a T3 session's VM). They are files of literal argv lists, not computed.
- **Callers.** `home.go` (`backend()`, `newAgent`, `SweepAgent`, `newSetup`), `agentvm.go` (the session server runs only where the backend is available), `t3login.go`, `setup` (`Options.Backend` replaces `GOOS`; `s.mac()` is gone), `features.go` (`agent` and `t3` ask `Backend.Available`), `t3vm.SessionSpec` (no tool name of its own: `Bin` is required) and the `agent-vm` shim (`ClientOptions.ExecTool`, the backend's tool; still `syscall.Exec` on Unix, the stdio relay of P6 elsewhere).
- **Still Apple's, for P9 to widen.** The tool *install* step (`setup/container.go`: download, signature, administrator install) is the Apple package's own and is not behind the `Provisioner`; a backend with another installer (QEMU, `virtiofsd`, KVM access) needs an `Install` there. The shim hands the exec to the tool itself; for a guest agent Home must attach the shim's stream to a guest exec (open question 5 of 2.7).
- **Allow-list.** Empty: all 13 lines are gone.

### P9 MicroVM backend for Linux

- **Status (spike done, owner's decisions of 2026-11-05).** The open questions of 2.7 are answered in `docs/design/linux-microvm.md`: QEMU/KVM with virtio-fs (virtiofsd) and a virtio-serial control channel; QEMU and virtiofsd are pinned builds signed with the release key, like T3 and Node; virtiofsd runs with `--sandbox none` under a Landlock rule set of the launcher (Ubuntu 24.04 gives unconfined programs no capable user namespace). Built so far: `internal/home/agent/microvm` (the QEMU and virtiofsd command lines, the guest's mount table, the KVM check) and the guest agent (`cmd/rubi-guest`, `internal/home/agent/guest`: PID 1, mounts, processes with stdio over the control channel, and Home's client), with their tests; the launcher (`microvm.Launcher`: the VM folder, virtiofsd confined by Landlock through Home's `vm-confine`, QEMU, pid files, the sweep), tested with fake programs and, on Linux, under the real confinement. A live test (`internal/home/agent/microvm/live`, tag `kvmlive`) boots a VM with the exact command lines on a test machine with KVM: the firmware (qboot, named with `-bios`), the PVH kernel, virtiofsd's flags, the control port and the end by the guest's restart are checked. Not yet: the CI builds (kernel, root file system, QEMU, virtiofsd), the backend, T3 sessions, and the live checks that need Rubi's own kernel (mounts, exec, no network, a kill leaves nothing).
- **Work.** A spike first, to answer the open questions of 2.7 (KVM access, virtio-fs or 9p, shipping QEMU and
  `virtiofsd`, vsock or serial); then the guest agent, the CI build of the kernel and root file system, the backend,
  the setup wizard steps (KVM present, QEMU present or installed, image downloaded and checked), and the sweep.
- **Done when.** On a Linux box with KVM an agent runs a project in a QEMU VM with no network device, the proxy and the
  hook gate work through the control channel, the role shield mounts are read-only inside, and a T3 session runs.
  Without KVM the feature says "agents need hardware virtualisation" and nothing else breaks.
- **Tests.** Unit tests for the QEMU argv (no `-nic` other than none, `-sandbox on`, the mounts and their modes, a quoted
  path); guest agent tests over a pipe; the netpipe tests reused for the control channel; a live test on a Linux box
  with KVM (boot, exec, no network, a read-only mount refuses writes, a kill leaves nothing); the secrets guard for
  the mounts. This is security-sensitive: `reviewer` and a second opinion before it merges.

### P10 T3 and Node pins (done)

- **What upstream publishes** (read from the release's `SHA256SUMS` and from `nodejs.org/dist/v26.13.0/SHASUMS256.txt` on 2026-11-04; the zips' layouts from their central directories by range requests, nothing large was downloaded). Every nightly of the last weeks has the same assets:
  - T3 runtime archives: `darwin-arm64`, `linux-arm64`, `linux-x64` (`.tar.gz`), and `win32-arm64`, `win32-x64` (`.zip`, 113 MB, one top folder, `t3.exe` inside, only folders and regular files). **No `darwin-x64` runtime archive**: the Intel Mac has the desktop app (`.dmg`), which Rubi Home cannot use. So Intel Macs get no pin (stopped there, as asked) and T3 says "not available on this computer".
  - Node: `darwin-x64.tar.gz` and `win-x64.zip` exist with sums (`node.exe` at the top of one folder).
- **Pins** (`t3/upstream.json`): `win32-x64` added to the T3 sums (`2ffef4de...`) and the Node sums (`dcc60d59...`, Node's `win-x64.zip`). Nothing for `darwin-x64`; the `win32-arm64` zip exists upstream too and can be added the same way when there is a Windows on Arm test machine.
- **`t3bump`**: `Platforms` gains `win32-x64`; `ArchiveName` (`.zip` for win32) and `NodeArchive` (`win-x64`) name the files; `NoBuild` lists `darwin-x64` with the reason and every bump looks for such an archive (`NowPublished`) and says so in its report, so the day upstream adds one the platform moves into `Platforms`; `CheckNode` stops a bump when Node has no sum for a platform (the bump moves T3 only). The `t3-bump` workflow first runs the pin-coverage tests.
- **Home** (`t3install.go`): `t3Platform` knows `darwin-x64` and `win32-x64`; `t3Unavailable(pin, plat)` gives the reason a platform has no T3 (no build for the system, upstream publishes none, this build pins none); `Install` refuses a no-build platform before any download with "That isn't available on this computer (...)"; `unpackZip` (same rules as `unpackTarGz`: nothing leaves the folder, no backslash, drive or stream names, no links or special files, sizes counted as read); the Windows layout (`node/node.exe`, `t3.exe`); the status says `supported` and `unsupported_reason`.
- **Gate.** `FeatureT3` is available only where there is a VM backend **and** a T3 build for the platform (`FeatureEnv.T3Platform`): an Intel Mac has no backend at all (see below), so agents and T3 both say "agents need a Mac with Apple silicon"; the T3 reason stays for the case of a backend without a T3 build.
- **T3 on Windows.** Pinned and installable (the zip path is tested), but not offered: T3's sessions run `claude` in a VM through the `agent-vm` shim, so T3 without agents does nothing, and the gate says "agents don't run on Windows yet" until P12 gives Windows a backend. Nothing else is needed then: the pin is already there.
- **Not verified:** the Windows install against the real zips (nothing downloaded; the layout was read from their directories, the tests use small zips of that shape), which only a real Windows machine shows.
- **Intel Macs (owner's decision, after P11).** Apple's `container` tool runs only on Apple silicon (its README: "You need a Mac with Apple silicon"). So on macOS the platform backend asks the kernel (`hw.optional.arm64`, the hardware, so an Intel build under Rosetta on Apple silicon still counts) and on an Intel Mac is `NoBackend("agents need a Mac with Apple silicon")`: agents and T3 are `not_available_here` with that sentence in the panel. Tests: `TestAnIntelMacHasNoAgents`, `TestAppleSiliconReadsTheHardware` (agent), the "an Intel Mac" case of `TestFeaturesPerSystem`, and `TestFeaturesOfThisSystem` expects agents exactly where the kernel says Apple silicon.

### P11 Release and Bot installation (the first Windows release) (done)

- **Release** (`release.yml`): a job `windows-home` builds `rubi-home-<tag>-windows-{amd64,arm64}.zip` (one file, `rubi-home.exe`, `CGO_ENABLED=0 GOOS=windows`, reproducible zip), a job `windows-check` runs the amd64 exe's `version` on a Windows runner, and `release` waits for both, downloads the zips into `dist`, and lists them in the signed `SHA256SUMS` (the `rubi-*.zip` glob already covered them) and the attestation. Not Authenticode-signed. The feed (`latest.json`) is unchanged: Windows has no download button, the Bot's way is the only one, so it needs no file name there.
- **`panel/install-home.ps1`** (served at rubi-panel.com like `install-home.sh`): constant; the only input is the optional release tag, checked by an anchored pattern (`\z`) before use. The environment can change nothing unless `RUBI_INSTALL_TEST=1` is set, and then the feed and download addresses must be this computer. Steps: pick the zip by CPU; download the signed list, its signature and the zip into memory (https only; a redirect to http is refused, also for the feed); check the signature with `Test-Ed25519` and the release key embedded in the script (`#region ed25519`: strict RFC 8032, canonical A and R, S below L, cofactorless; `System.Numerics` loaded by assembly name; no program is run, nothing is written to disk before the check); read the archive's checksum from the same signed bytes; hash and unpack the zip from the same bytes (a `ZipArchive` over a `MemoryStream`; no zip file on disk). `rubi-home.exe` is taken from the zip by name; an upgrade ends a running copy by its path, moves the old file aside, moves the new one in (the old one is put back if that fails), names any `.old-*` file it could not delete, and `rubi-home install-autostart` (which ends the Scheduled Task's running copy and starts the task again; P4's Scheduled Task, no elevation) follows; then `rubi-home open` for pairing. Files, hashes, big numbers and downloads are .NET calls, not cmdlets that PowerShell would autoload from the user's module folder. The owner decided: Windows has no OpenSSL and no Git; the installer checks Ed25519 itself. `scripts/test-install-home-ed25519.ps1` runs the script's own code (taken from it by the PowerShell parser) on 242 vectors in `internal/repocheck/testdata/ed25519` (RFC 8032 7.1, all 210 of Wycheproof's `ed25519_test.json`, 12 of ed25519-speccheck, 15 derived; `valid` from a separate reference written from RFC 8032 section 6), and the release job runs it on the real `SHA256SUMS` and `.sig` before `gh release create`.
- **`SKILL.md` (install.md)**: macOS and Linux and Windows subsections; Windows says what the Bot downloads and runs, that the installer needs nothing installed first (it checks the signature itself); the download of the script itself (`-UseBasicParsing`, TLS 1.2, a random file name, deleted afterwards) and the statement that it is trusted through TLS to rubi-panel.com, that the user confirms nothing for Home itself, how the P3 window shows the PIN (and what to do with no window), and that UAC comes only later for the hook ("Unknown publisher" is expected). "Windows is not supported yet" is replaced by what each system has: Home, pairing, Hue, the hook and the sister tools everywhere; Shortcuts on a Mac; agents and T3 on a Mac only for now.
- **Panel** (`homeInstallSteps`): the same Bot message for every system; texts for Windows (the window, the hook's prompt) and for what is and is not available; no command anywhere. **`install-home.sh`**: the hook-update line no longer says macOS on Linux, the Shortcuts hint is printed on a Mac only, and a Windows shell (Git Bash) is told to use the `.ps1`.
- **Tests.** `repocheck/installhomeps1_test.go`: the script's steps in order (signature, then checksum, then unpack, install, autostart, pairing last), the signature checked on the bytes that are used and nothing written before it, the embedded key equals `release-key.pub.pem` and the shell script's, no program run for the check (no OpenSSL, no `ProcessStartInfo`), constant (no evaluation, fixed environment names, https only), balanced syntax, a PowerShell parse when `pwsh` exists, the checksum-line pattern; `vectors.json` against Go's `crypto/ed25519` (all agree but the two known non-canonical-key cases) and its make-up; the vector script run with `pwsh` when it exists; the workflow shapes; the Windows section of install.md. CI: `home-windows-install` runs the script on a Windows runner against a fake release signed with a throwaway key (signed there with Git's `openssl`, which is CI only) served from loopback, and checks the refusals: bad signature, a malleated signature (S + L), bad checksum, the environment ignored without the test flag, a test address that is not this computer. `home-ed25519` runs the vector script under Windows PowerShell 5.1 and under `pwsh` on Linux. The release's `windows-check` also looks at each zip (one entry `rubi-home.exe`, the machine type of its CPU, so the arm64 zip is checked without running it).
- **Only a real Windows machine shows** (nothing here ran Windows PowerShell 5.1; `pwsh` 7 ran the vectors and the real v0.14.0-rc.2 signature): the script parses and runs under Windows PowerShell 5.1 (the CI jobs do this on a runner, and how long the check takes there); a Scheduled Task created by a non-elevated user at logon; the P3 pairing window showing from a script started by a Bot (a session that is not the interactive one prints the link and PIN instead); the UAC prompt for the hook; SmartScreen/Defender on an unsigned `rubi-home.exe` downloaded by a script; the release workflow's Windows jobs (never run).

- **Release-version rule (owner's decision, after P11).** `latest.json` was not signed, so whoever could change it could name an older, genuinely signed release and the installers would install it (a rollback). Now `pages.yml` signs it into `latest.json.sig`; every installer and updater takes the version only from the verified feed, requires the signed `SHA256SUMS` to carry that exact version, and refuses a release older than the installed one, with no override. Written once in `docs/design/rubi-home.md` ("The release-version rule"); tests are listed there. Also added: `update.Download` refuses a version that is not newer, and the hook install refuses to replace a newer hook. The feed gets no expiry (owner's decision): a replayed older feed can only affect a fresh install, and whoever can replay it can also serve a changed installer; the risk is written down in the design note. CI checks the newest published release's real signature with the PowerShell verifier (`home-ed25519-windows` on Windows PowerShell 5.1, `home-ed25519-pwsh`).

### P12 MicroVM on Windows (after the first Windows release)

- **Work.** QEMU with WHPX, a file service over the control channel for the project folder (no virtfs on Windows
  hosts), the setup steps for the Windows Hypervisor Platform feature (an administrator step, UAC), path translation.
- **Done when.** An agent runs on a Windows 11 test machine and the project folder appears at `/work`.
- **Tests.** The Linux backend's argv and agent tests with the Windows differences; the file service against the deny
  lists; a live test on Windows.

### P13 Scripts folder for Linux and Windows (later)

- **Work.** The counterpart of Shortcuts: a folder of programs the user lists; Rubi runs only those, by name, with the
  consent rules of Shortcuts. Its own design note before the work starts.
- **Done when.** A Bot can list and run a user's script on Linux and Windows, and the macOS Shortcuts feature is
  unchanged.
- **Tests.** As `shortcuts_test` for the listing and the refusal of anything not in the folder.

### P14 Close-out

- **Work.** Empty the allow-list, update `docs/design/rubi-home.md`, `docs/design/security-model.md` (what the boundary
  is on each OS), `docs/project-rules.md`, `README.md`, the changelog.
- **Done when.** `homeplatform_allow.txt` has no lines; the cross-build runs on every push.
- **Tests.** The guard test with an empty list.

## 4. Risks

- **Windows is the largest piece.** Home does not build there now, the root `sh` script has no counterpart, and the
  socket security model changes. P6 is the one where a mistake is a security hole (a process of another user passing for the
  hook).
- **A new VM backend is a large, security-sensitive piece (P9).** It needs QEMU and `virtiofsd` shipped or required, KVM
  access, and a guest agent of our own. Firecracker was rejected for its lack of shared folders; if virtio-fs proves
  unworkable the project-folder story needs rethinking.
- **No KVM, no agents.** Hosts without hardware virtualisation (many VPSes, some boards) can not run agents on Linux.
- **Windows has no agents at first**, and later needs a file service of our own.
- **polkit text and headless boxes.** The password dialog can only name `/bin/sh`.
- **No Authenticode (decision 4).** The UAC prompt says "Unknown publisher", and SmartScreen may warn on a downloaded
  `rubi-home.exe`. The Bot's download does not carry the web mark, but a user who downloads it by hand will see the
  warning. For this reason there is no no-Bot Windows installer.
- **The Ed25519 check on Windows.** Decided by the owner: the installer checks the signature itself, in PowerShell over
  .NET's `BigInteger`, so nothing else must be installed. It follows RFC 8032 strictly (canonical key and R, S below L,
  cofactorless), which Go's `crypto/ed25519` does not in two cases (a non-canonical key). The risk is a bug in code that
  nobody else uses; the vectors and the release job are the answer (P11).
- **The Bot's `sudo` on headless Linux (decision 2).** Consent is the panel approval, not a password. The skill and the
  card must say exactly what is run, and the root side must refuse any plan whose hash differs.
- **Windows file semantics.** Case-insensitive paths, reparse points, 8.3 names, sharing violations on rename of a
  running exe (an update must rename the old exe away, as the OS does not allow overwriting it).
- **Claude Code's own paths on Windows** (managed settings, the hook shell) are not read here from a Windows box; they
  are taken from its docs and need a check.
- **Maintenance.** Three install paths, three VM backends (Apple, QEMU on Linux, QEMU on Windows): the guard test and the three-OS CI cross-build are what keep
  them from drifting.
- **The guard is a net, not a proof.** It finds literals; code that builds `container` from a variable passes. The
  cross-build and review catch the rest.

## 5. Decisions

The owner's answers to the questions of the first version of this plan:

1. **Windows.** The first Windows release is Home without agents: hook, Hue, sister tools, pairing. Agent VMs come later
   (P12).
2. **Headless Linux.** With no polkit agent, the Bot installs the hook with `sudo` after the user's approval in the panel;
   the approval shows the command's hash (2.4).
3. **Agent isolation.** A real VM on all OSes: a new QEMU/KVM (or Firecracker) backend, not rootless Podman. Apple
   `container` stays on macOS (it is a real VM). The open technical questions are in 2.7; stages P8, P9, P12.
4. **No Authenticode.** Windows: the installer checks Ed25519 itself (RFC 8032, Wycheproof and speccheck vectors in CI, the
   real release signature checked in the release job); no OpenSSL, no Git. (This replaces the earlier decision to verify
   with `openssl` from Git for Windows.)
5. **Folder chooser.** The project folder is chosen with a folder browser in the panel, which also works from a phone
   (2.2, P3).
6. **Shortcuts.** Stays macOS-only for now; a scripts folder for other OSes comes later (P13).
7. **Linux packaging.** tar.gz plus the Bot's install; no `.deb` or `.rpm`.
8. **Windows dependencies.** Only `golang.org/x/sys`; `go-winio` only if nothing else works, and with the owner's consent.
9. **T3 for Intel Macs.** Add `darwin-x64` in P10.

### Still open

- Anything in the list of technical questions in 2.7 (the spike of P9 answers them).
- Whether the Windows scheduled task can be created without elevation (2.3), and Claude Code's Windows paths (2.1).
