# Rubi Home on macOS, Linux and Windows (plan)

Rubi Home is built for a Mac. The owner wants it on Linux and Windows too. This plan does not port anything. It
records what ties Home to macOS today (the audit), what replaces each piece on the other two systems, and the stages
in which to do it. `internal/repocheck/homeplatform_test.go` already keeps new code from adding to the list.

Project rule 2 (the user never uses a terminal) holds on all three systems: the Bot installs Home, and the user only
confirms what the system itself asks for.

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
  buttons...) (button)`, `Dialogs.ChooseFolder(prompt)`. `OpenEnv` and `Setup` take them; the default is the one for
  `runtime.GOOS`.
- **macOS (unchanged).** `open`, `pbcopy`, `osascript` `display dialog` and `choose folder`.
- **Linux.**
  - Open: `xdg-open <url>`. It needs a desktop session (`DISPLAY` or `WAYLAND_DISPLAY`); without one, return
    "no display" and fall back to text.
  - Clipboard: `wl-copy` on Wayland, `xclip -selection clipboard` or `xsel -ib` on X11, code on stdin. None found: no
    copy button; the code is shown instead.
  - Dialog: `zenity` or `kdialog` when present (text, up to three buttons, folder chooser). Otherwise the text mode of
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
- **Folder chooser, for all three.** A native chooser per OS is the weak part (Linux needs zenity, Windows needs a COM
  dialog). A different way works the same on every OS and on a phone: Home answers `fs.list {path}` for folders under
  the user's home (refusing the deny lists), and the panel shows a folder browser. Open question 5.
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
  - No polkit agent (a headless box): `pkexec` cannot ask. Open question 2.
  - The script's tool table already has the `stat -c`, `sha256sum`, `root` row; add a test that runs it under `sh`
    against a temporary root.
- **Windows: UAC.**
  - Home starts `rubi-home.exe install-hook --elevated --plan <file>` with `ShellExecuteEx` and the `runas` verb.
    Windows shows its UAC prompt on the secure desktop. The prompt names the publisher of the signed binary; an
    unsigned one shows "Unknown publisher" in a yellow bar, so signing matters (open question 4).
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
  - No new module is needed: `golang.org/x/sys` is already used; `go-winio` would be convenient but needs the owner's
    approval (project rule: no new dependency without approval).
- **Locks.** `lockFile` and the two `Flock` uses become `lockFile` with `_unix.go` (flock) and `_windows.go`
  (`LockFileEx`). `O_NOFOLLOW` opens go behind `openNoFollow(path, flags)`: Unix uses the flag; Windows opens with
  `FILE_FLAG_OPEN_REPARSE_POINT` and checks the attributes. The hard-link count (`shield.go:207`) is
  `GetFileInformationByHandle.NumberOfLinks` on Windows.
- **Process control.** `Setpgid` and `Kill(-pid)` in `hookcheck.go` become `runWithKill(ctx, cmd)`: Windows uses a Job
  Object (kill on close).
- **Shim.** `agent-vm` replaces itself with `container exec` by `syscall.Exec`. Windows has no exec: the shim starts the
  child with the same stdio and exits with its code (`client_other.go` already marks the gap).

### 2.6 Features that stay on macOS

- **Shortcuts and Apple Home** need the Shortcuts app and Apple Home: macOS only. No port is planned. A scripts
  folder of programs the user lists (Rubi runs only those) could be the Linux and Windows counterpart; it is a new
  feature, not part of this plan (open question 6).
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
- **Hue** works everywhere.

### 2.7 Agent VMs

The `VM` interface (`agent/vm.go:135`) is the seam: `Start`, `Exec`, `Stop`, `State`. What is Apple-specific is the
implementation (`AppleContainer`), its installer and the `container` argv in the setup and shim code. The plan:

- **Interface.** `VMBackend{Name() string; Probe(ctx) Step; Install(ctx) job; Build(ctx, project) ...; New(project) VM;
  ExecArgv(...) []string}`. `home.go:newAgent`, `setup`, `t3login` and the `agent-vm` shim ask the backend for argv instead
  of writing `container`. `agent/apple.go` becomes `apple_darwin.go`; a `cli.go` holds the part that is shared by
  every OCI CLI.
- **Does Apple `container` have a Linux counterpart the code could reuse?** No. It is a macOS 26 and Apple silicon
  tool that runs each container in its own lightweight VM through the Virtualization framework. Its CLI is close to
  Docker's, which is the useful thing: the arguments `AppleContainer` builds (`run -d --name --init --network none
  --user --cap-drop ALL --read-only --tmpfs /tmp --mount type=bind,source=,target=[,readonly]`, `exec -i`, `image
  inspect`, `stop`, `rm`) are valid for `podman` and `docker` as they are, apart from `build --progress plain`
  (Docker has `--progress=plain`; Podman has no such flag, drop it) and `system start`, which has no equivalent.
  So the code shared with a Docker-style backend is most of `RunArgs`, `exec`, `netpipe` and the image build.
- **What the owner wants from a backend is a VM boundary** (the design says "VM", `docs/design/agent-runner.md`): the
  agent runs code the Bot wrote, with the user's project mounted. Survey:

| Backend | System | Boundary | Fits the interface? | Notes |
|---|---|---|---|---|
| Rootless Podman | Linux | container (shared kernel, user namespaces) | Yes, same argv | In every distro's repos, no root, no daemon. Weaker than a VM. Add `--security-opt no-new-privileges`, `--userns keep-id` or a fixed uid, seccomp default. Mounts: `:Z` label on SELinux hosts. |
| Podman with the Kata or gVisor (`runsc`) runtime | Linux | microVM (Kata) or user-space kernel (gVisor) | Yes, `--runtime` flag | Closest to Apple's per-container VM. Kata needs KVM; gVisor does not. Extra install step. |
| Docker Engine, rootful | Linux | container | Yes | Needs root and the docker group: the group is root-equivalent; avoid as default. Accept if present. |
| Firecracker | Linux with KVM | microVM | No: needs a rootfs image, a kernel, a vsock agent and our own exec; no OCI CLI | A new backend written from scratch. Strong, big. |
| QEMU/KVM with a prebuilt image | Linux, Windows (WHPX), macOS | VM | No: same as Firecracker, plus 9p/virtiofs mounts | Portable but the heaviest to build and ship. |
| Podman machine or Docker Desktop | Windows | one shared Linux VM (WSL2 or Hyper-V) running containers | Yes, same argv | One VM for all containers: container boundary inside a VM boundary against Windows. |
| WSL2 distro driven by `wsl.exe` | Windows | shared WSL2 VM | Partly: Home runs `podman` inside the distro; project mounts are `/mnt/c/...` (9p, slow, permissions) | Needs WSL2 installed (admin, reboot possible). |
| Hyper-V VM | Windows Pro and up | VM | No (as Firecracker), not on Home editions | Skip. |

- **Recommendation.** Linux: rootless Podman by default (same argv as today's backend), Kata or gVisor as an
  upgrade when present. Windows: Podman through WSL2 (`podman machine`), the installer checking WSL2 first. Firecracker and
  QEMU only if the owner wants a true VM on Linux and Windows (open question 3).
- **Network.** `--network none` plus `rubi-netpipe` is portable Go; it needs only `exec -i`.
- **Arch.** The image's claude binary follows the host (`t3vm/server.go:53`): make it `linux-arm64` or `linux-x64` from the
  image, not a constant.
- **Installing the backend.** macOS: Apple's signed package (exists). Linux: the package manager's `podman`, which
  needs root: the same `Elevator` (pkexec) runs `apt`, `dnf` or `pacman`, chosen from `/etc/os-release`, with a pinned
  list of package names; no download of unsigned code. Windows: `winget install RedHat.Podman` or the MSI, checked
  by Authenticode and SHA-256, through UAC; `podman machine init` after.

### 2.8 T3 and Node

- `t3Platform()` knows `darwin-arm64`, `linux-arm64`, `linux-x64`. Add `darwin-x64` (Intel Macs) and `win32-x64` to the
  pins (`t3/upstream.json`, `t3bump.Platforms`) once upstream publishes them; Windows archives are `.zip`, not
  `tar.gz` (`t3install.go:418`, `:446`).
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
| Installer checks | signed `SHA256SUMS`, then the archive hash | the same | the same; the Ed25519 check needs a verifier on Windows (open question 4) |
| Autostart | LaunchAgent | `systemd --user` | Scheduled Task |
| Hook | the panel button, administrator dialog | the panel button, polkit dialog | the panel button, UAC prompt |
| User confirms | nothing at install; the administrator password in the dialog when the hook is installed | the polkit password when the hook is installed | the UAC prompt when the hook is installed (and for WSL2 or Podman, if agents are set up) |
| Pairing | `rubi-home open` shows the window: PIN and link; Open the Rubi panel | `rubi-home open` shows a zenity window with the PIN when there is a desktop; otherwise prints the link and PIN and the Bot sends them in chat | `rubi-home open` shows a TaskDialog with the PIN and link and the same three buttons |
| Without a Bot | the app zip (exists) | none (allow-list T-01L) until a package exists | a signed `Rubi Home Setup.exe` that does what the app does |

`install.md` says what the user will see ("Windows will ask whether to allow this app to make changes: choose Yes"),
and that the Bot does not run `sudo` or open an elevated shell for the hook (the consent is the user's own prompt).
The skill text is owned by the work on `skills/`; this plan lists what it must say.

The PIN rule: the PIN the user types in the panel must come from Home's window where there is one. In text mode
(no display) the Bot sees it. Pairing still needs the user's passkey approval in the panel, so a Bot that pairs
itself would stop there.

### 2.10 Release artifacts

- **macOS.** Unchanged: `rubi-home-<v>-darwin-{arm64,amd64}.tar.gz`, `rubi-home-<v>-macos-universal.zip` (the app),
  in the signed `SHA256SUMS`.
- **Linux.** `rubi-home-<v>-linux-{amd64,arm64}.tar.gz` (exists). Later and optional: a `.deb` and an `.rpm` (the
  package installs the user unit file in `/usr/lib/systemd/user/` and a polkit-less hook folder), an AppImage only if
  the no-Bot path is wanted.
- **Windows.** `rubi-home-<v>-windows-{amd64,arm64}.zip` with `rubi-home.exe` (built `CGO_ENABLED=0 GOOS=windows`),
  plus `rubi-home-<v>-windows-setup.exe` for the no-Bot path (an installer program that puts `rubi-home.exe` in
  `%LOCALAPPDATA%`, makes the Scheduled Task and opens the pairing window; Authenticode-signed). All in the signed
  `SHA256SUMS`.
- **Workflow.** A Windows job (`windows-latest`) builds and signs the exe and the setup program (as the `mac-app` job does
  for the app); `release.yml:83` gains `windows/amd64 windows/arm64`; the cross-build job for the test (P1) runs on
  every push.
- **Feed.** `releases/latest.json` (read by `homeMacAppURL`, `app.js`) gains a Windows file name.

## 3. Stages

Each stage ends with `go test ./...` green, the allow-list shorter by the lines the stage removes, and a changelog
line. Stage names only; the order is the dependency order.

### P1 Cross-build baseline

- **Work.** Split the Unix-only code so that `GOOS=windows go build ./cmd/rubi-home` and `GOOS=linux`, `GOOS=darwin` pass:
  `lockFile`, `openNoFollow`, the `Stat_t` reads, `Setpgid`, `Flock` and the `Nlink` count get `_unix.go` and
  `_windows.go` files (the Windows ones may return "not supported" for now). A CI job runs `go vet` for the three
  systems.
- **Done when.** The cross-build passes on all three and a Windows build of `rubi-home version` runs on a Windows host.
  No allow-list line is removed; the stage adds none.
- **Tests.** The CI job; unit tests for the new `_unix.go` helpers on this host; a `repocheck` test that the cross-build
  files exist.

### P2 Platform paths

- **Work.** 2.1: `platformPaths()`, per-OS deny lists, the trusted-file check.
- **Done when.** No source outside `paths_*.go` names `~/Library`, `/Library` or `/usr/local/bin/rubi-home`; the
  Linux `DataDir` is unchanged; Windows paths resolve in a table test.
- **Tests.** Table tests per OS with a fake environment; the hook owner check against files made in a temp folder;
  a test that `DefaultDir` on Linux still returns the old path. Removes the P2 lines of the allow-list.

### P3 Opener, clipboard and dialogs

- **Work.** 2.2: `Opener`, `Clipboard`, `Dialogs`; the Linux and Windows implementations; text mode as the fallback.
- **Done when.** `rubi-home open` shows the PIN and the link in a window on a Linux desktop and on Windows, and prints
  them where there is none; `login.go` opens the browser through `Opener`.
- **Tests.** Fake runners for the argv of each tool; the existing `openwindow_test.go` cases run for each OS table; a
  manual check on a Linux desktop and on Windows 11. Removes the P3 lines.

### P4 Autostart and the app flow

- **Work.** 2.3: `Autostart` with launchd (moved), systemd user and Scheduled Task; `install-autostart`; the shell
  installer calls it; `IsAppLaunch` moves to `app_darwin.go` with a `false` for the others.
- **Done when.** On each OS Home starts at login, restarts after a crash, and an update restarts it.
- **Tests.** Fake `Run` tests for the launchd calls (exist), the systemd calls and the task XML; unit file and task
  XML golden files; a manual log-out and log-in on each OS. Removes the P4 lines.

### P5 The administrator step

- **Work.** 2.4: `Elevator` with `osascript`, `pkexec` and UAC; the Windows Go install code; the tool table per OS;
  the managed-settings path in the plan output.
- **Done when.** The panel's "Install the Claude Code hook" works on each OS with the system's own prompt, and a
  cancelled prompt gives `ErrAdminCancelled` and the panel's "cancelled" text.
- **Tests.** The root script on Linux under `sh` in a temp root; the Windows install code against a temp folder with
  the ACL check; the cancel and error mappings with fakes; manual runs on each OS. Removes the P5 lines.

### P6 Local sockets and peers on Windows

- **Work.** 2.5: `ListenLocal`/`DialLocal`; named pipe (or AF_UNIX) with DACL; `osProcs` for Windows in `internal/home` and
  `agent/t3vm`; Job Object kill; the `agent-vm` shim without exec.
- **Done when.** The hook, `claude-mcp` and `claude-send` talk to Home on Windows, and a process of another user, or one
  that is not the installed hook, is refused.
- **Tests.** Pipe tests in the same process and across two processes on a Windows runner; a second-user test by hand.
  This is the security-sensitive stage: it needs the `reviewer` and a second opinion before it merges.

### P7 Feature availability

- **Work.** 2.6: `Features()`, `not_available_here`, `can` computed, the panel lines, the skill text, `shortcuts.go`
  behind `_darwin.go` and a stub elsewhere; the Claude desktop link behind `Opener` and the handler check.
- **Done when.** On Linux the panel and a Bot see "Shortcuts: not available on this computer" and never an English
  CLI error.
- **Tests.** Core and Home tests for the typed error; panel flow test (`panelflows_test.go`); a Bot-facing text test.
  Removes the P7 lines.

### P8 VM backend interface and Podman

- **Work.** 2.7: `VMBackend`; move `AppleContainer` to `apple_darwin.go`; `PodmanBackend` for Linux; setup steps
  from the backend; arch from the image.
- **Done when.** On Linux a project's agent runs in rootless Podman with `--network none` and the netpipe, and the
  setup wizard installs Podman through the `Elevator`.
- **Tests.** Argv tests that compare the Apple and Podman argv; the existing VM fake tests run against both; a live test
  on a Linux box with Podman (build the image, run the runner, prove no network). Removes the P8 lines.

### P9 VM backend on Windows

- **Work.** Podman on WSL2 (`podman machine`) or Docker Desktop; path translation for mounts; the WSL2 check in setup.
- **Done when.** An agent runs on a Windows 11 test machine and the project folder is mounted at `/work`.
- **Tests.** Argv and path-translation unit tests; a live test on Windows; the guard for secrets in mounts.

### P10 T3 and Node pins

- **Work.** 2.8: pins for `darwin-x64` and `win32-x64`, zip extraction, `t3bump` platforms.
- **Done when.** T3 installs and pairs on the new platforms, or says "not available on this computer".
- **Tests.** `t3install_test.go` tables for each platform; `t3bump` tests; the existing zip-slip tests for zips.

### P11 Release and Bot installation

- **Work.** 2.9 and 2.10: Windows jobs, artifacts, feed, `install-home.sh` fixes (texts), `install-home.ps1`, the
  `install.md` subsections, panel texts for Linux and Windows, `usertext_allow.txt` T-01L closed for the systems that now
  have a no-terminal path.
- **Done when.** A Bot on a fresh Linux box and a fresh Windows machine installs Home, the user sees only the system's
  prompts, and pairing completes.
- **Tests.** `repocheck` tests for the workflow and the feed (like `homeapp_test.go`), `install.md` text test (no
  command for the user), a clean-machine run on each OS.

### P12 Close-out

- **Work.** Empty the allow-list, delete the stage headings' "removes" notes, update `docs/design/rubi-home.md`,
  `docs/project-rules.md`, `README.md`, the changelog.
- **Done when.** `homeplatform_allow.txt` has no lines; the cross-build runs on every push.
- **Tests.** The guard test with an empty list.

## 4. Risks

- **Windows is the largest piece.** Home does not build there now, the root `sh` script has no counterpart, and the
  socket security model changes. P6 is the one where a mistake is a security hole (a process of another user passing for the
  hook).
- **Weaker boundary on Linux and Windows.** Rootless Podman shares the kernel; a Windows backend adds a VM
  under the containers, but one shared VM. The design says "VM". The panel and `docs/design/security-model.md` must say
  what the boundary is on each OS.
- **polkit text and headless boxes.** The password dialog can only name `/bin/sh`.
- **UAC and signing.** An unsigned `rubi-home.exe` shows "Unknown publisher" and SmartScreen warns on download.
- **Verifying the release on Windows.** `install-home.sh` uses `openssl` for Ed25519; Windows has none.
- **Windows file semantics.** Case-insensitive paths, reparse points, 8.3 names, sharing violations on rename of a
  running exe (an update must rename the old exe away, as the OS does not allow overwriting it).
- **Claude Code's own paths on Windows** (managed settings, the hook shell) are not read here from a Windows box; they
  are taken from its docs and need a check.
- **Maintenance.** Three install paths, two VM backends: the guard test and the three-OS CI cross-build are what keep
  them from drifting.
- **The guard is a net, not a proof.** It finds literals; code that builds `container` from a variable passes. The
  cross-build and review catch the rest.

## 5. Questions for the owner

1. **Windows scope.** Home on Windows without agents (hook, Hue, sister tools, pairing) first, with agent VMs later (P9)?
   Or are agents a must for the first Windows release?
2. **Headless Linux hook.** On a box with no polkit agent there is no password prompt to show. Allow the Bot to run
   `sudo` for the hook install there (the user's consent is then the panel approval that shows the hash), or require a
   desktop session for the hook?
3. **VM strength.** Is a container boundary on Linux (rootless Podman) acceptable, with Kata or gVisor as an option, or
   must every OS have a real VM (Firecracker or QEMU, which is a new backend)?
4. **Windows signing and verification.** Buy an Authenticode certificate (EV or standard), and how should the installer
   verify the Ed25519 release signature without `openssl`: a small signed verifier built into the setup exe, or the
   `openssl` that Git for Windows ships (Claude Code on Windows needs Git)?
5. **Folder chooser.** Build a folder browser in the panel (works on every OS and on a phone), or use native choosers per OS (zenity,
   a Windows dialog)?
6. **Shortcuts on other systems.** Stay macOS-only, or later offer a "scripts folder" for Linux and Windows?
7. **Linux package.** Is a `.deb` and `.rpm` wanted, or tar.gz plus the Bot enough?
8. **`go-winio`.** May the Windows named-pipe code use it, or only `golang.org/x/sys`?
9. **Intel Macs and T3.** T3 is pinned for Apple silicon only; add `darwin-x64` in P10?
