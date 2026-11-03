# Rubi Home without a terminal (plan: T-01, T-02)

Project rule 2 says the user never runs a command. Today three Rubi Home steps need one:
- installing Home (`curl ... | sh`);
- pairing (`rubi-home pair`);
- installing or updating the root-owned Claude Code hook (`sudo rubi-home install-hook`).

This plan removes all three. The tasks are listed in `docs/project-rules.md` ("Known gaps", rule 2). Each closed entry
leaves `internal/repocheck/testdata/usertext_allow.txt`.

## Stages

| Stage | Task | What |
|---|---|---|
| H1 | T-02 | Hook state in the panel, and an install or update button that opens the macOS administrator dialog |
| H2 | T-01 | Home pairs from its own window: a code to copy, or a link that opens the panel with the code filled in |
| H3 | T-01 | The release ships `Rubi Home.app` for macOS; opening it installs Home, starts it at login and goes to H2 |
| H4 | T-01, T-02 | Docs, the allow-list, the changelog, and a manual check on a test Mac account |

## H1: the hook from the panel (T-02)

- **Home.**
  - `hello` gains `hook`, which holds:
    - `state`: `missing`, `current`, `stale`, `insecure` or `dev`;
    - the hook's version;
    - whether the Claude Code plugin is installed.
  - It reuses `InspectHookBinary` and the version compare in `hookver.go`.
- **Home op `hook.install`.**
  - Runs the existing `InstallHook` in-process with the `osascript` runner. The administrator dialog appears on the
    Home computer; there is no TTY and no sudo.
  - The root script creates a missing hook folder itself, so no `sudo mkdir -p` text is needed.
  - It installs the plugin in the same dialog when the plugin is installed already, or when asked to.
  - It verifies the result as the CLI does: hash, version, ownership.
  - It returns the new `hook` state.
- **Core.**
  - New panel op `device.hook.install {device}`.
  - It needs an approval of kind `rubi.settings` (a passkey or password).
  - Its execute step starts Home's `hook.install`, which returns at once; the install itself runs on Home (see Status).
  - It is audited without secrets.
- **Panel.**
  - The Home list shows each computer's hook state.
  - It shows **Install the Claude Code hook** or **Update the hook** when that applies.
  - It says that macOS will ask for the administrator password on that computer.
- **Texts.**
  - `hookver.go`, `hookcheck.go` and `claudemcp.go` say "update the hook in the Rubi panel (Settings, Home)" instead of
    a command.
  - The `--sudo` path is removed.
  - `rubi-home install-hook` stays as a maintainer tool and is not named in any user text.
- **Checks.**
  - Home op tests run with a fake runner: they cover the state, a missing folder, a failure, and a refused dialog.
  - A core test: the op needs an approval, and the execute step calls Home.
  - Panel flow test (`panelflows_test.go`).
  - The T-02 entries leave the allow-list.

## H2: pairing without a command (T-01)

- **Home window.** When the Home app opens (H3) and on every later open, Home shows a native dialog (`osascript`
  `display dialog`, which needs no new dependency). Every dialog names the computer and its fingerprint.
  - **Not paired:** the dialog offers **Copy the code** and **Open the Rubi panel**.
    - A pairing code is made only when one of them is pressed (a still-good pending code is reused); Close leaves
      nothing live.
    - Copy puts the full code on the clipboard (stdin of `pbcopy`), so Universal Clipboard can carry it to a phone.
    - Open opens the panel with a code that holds no secret; the next dialog shows a PIN (`XXXX-XXXX`) to type there.
  - **Paired:** the dialog shows which Rubi it is paired with and the fingerprint. It offers **Pair again**, which
    goes back to the case above.
  - The code is one time, 15 minutes. The PIN is the secret of the link route; five wrong secrets void the pending
    code. Config updates take a file lock, so the CLI and the helper do not lose each other's changes.
- **Panel link.**
  - Open the Rubi panel opens `https://rubi-panel.com/#home-code=<code without secret>`.
  - On load, the panel reads the code, removes it from the address bar at once, and keeps it in local storage for at
    most 15 minutes (never a code with a secret; an expired one is deleted at every page start).
  - It tells the user the truth: if nothing could be stored, it says to copy the code from Rubi Home's window.
  - It opens the "Add a home computer" screen with the code filled in, once the user opens Rubi with their own link,
    shows "name · fingerprint" worked out on the client for comparing, and asks for the PIN.
  - Pair still needs the passkey approval, as today.
  - The code is deleted after use or expiry.
  - `device.pair` becomes a sensitive op (its args hold the PIN or secret).
- **Texts.**
  - "rubi-home status shows the same" becomes "Rubi Home shows the same fingerprint when you open it".
  - "start it with: rubi-home run" and "run `rubi-home pair` again" become "open Rubi Home".
- **Checks.**
  - Dialog code with a fake runner, covering the not-paired, paired and Pair-again cases.
  - The panel link test: the code leaves the URL, expires, and is used once.
  - Panel flow test.

## H3: `Rubi Home.app` (T-01)

- **The release** builds one `rubi-home-<version>-macos-universal.zip` (arm64 and amd64 joined with `lipo`).
  - It holds `Rubi Home.app`, with `Contents/MacOS/rubi-home` (the same binary) and an `Info.plist`.
  - The `Info.plist` sets `LSUIElement` (no Dock icon) and has a bundle id and a version.
  - The zip is listed in the signed `SHA256SUMS`, like every asset.
- **Opening the app** (Finder starts the binary with no arguments, from inside a bundle):
  1. copies the binary to `~/.rubi-home/bin/rubi-home`, the same place as the script;
  2. writes and loads the LaunchAgent, with the plist that `install-home.sh` writes;
  3. waits for the helper to answer;
  4. shows the H2 dialog.

  Opening a newer app later replaces the binary and restarts the helper: this is the update. After an update the panel
  shows **Update the hook** (H1).
- **The panel's "Add a home computer" screen.** On a Mac or a phone, it shows one **Download Rubi Home for Mac** link to
  the latest release's zip, with these steps:
  1. open the zip;
  2. open Rubi Home;
  3. if macOS says it can't check the developer, open System Settings, Privacy & Security, and press Open Anyway;
  4. press Pair here.
- **Linux** keeps the script line for now: there is no desktop installer. It stays on the allow-list as T-01L, and is
  shown only under "On Linux".
- **Checks.**
  - A test for the bundle layout, run on the script the release uses.
  - The first-run logic, tested with a temporary home directory and a fake `launchctl`.
  - The release workflow lists the zip in the sums.

## Open points for the owner

- **Developer ID.** Signing and notarizing with an Apple Developer ID would remove the "Open Anyway" step. It needs a
  paid Apple developer account, so it is the owner's choice. The release step will take the signing identity from
  secrets when they exist.
- **Updating the hook asks for the administrator password on the Home computer itself.** A phone can't answer it;
  macOS allows nothing else for a root-owned file.
- **H3 can't run on a Mac whose live Rubi Home must stay as it is.** The manual check uses a separate macOS user
  account or a VM, with the owner's OK.

## Status

- H1: done (hook state in `hello`, Home op `hook.install`, panel op `device.hook.install`, panel button, T-02 texts and allow-list entries removed).
  Review fixes (second pass):
  - `hook.install` starts and returns `{started: true}`; the install runs on its own 5-minute context, so the
    administrator dialog isn't killed with the 60 s request. `hello.hook` has `installing` and `last`
    (`ok`, `cancelled`, `timeout`, `failed`, plus a sentence). The approval's result says macOS is asking for the
    password; the panel polls `device.check` every 2 s (6 minutes at most).
  - Home refuses a new install for 2 minutes after `cancelled` or `timeout`, and when the hook is current and the
    plugin needs no change. The macOS dialog carries a prompt naming the request (argv item 2).
  - The target is `DefaultHookBinary` only; `hello` inspects the same file. Core asks Home's `hook.plan`
    (`{target, version, sha256}`, no prompt) before the approval and shows it.
  - Refusals are a code (`unsafe_dir`, `not_plain`, ...) and a fixed sentence; paths stay in Home's log.
  - `hello.hook` also has `why` (`symlink`, `owner`, `dir`) and `fixable`; the panel offers the button only when
    fixable. `hello` does not hash the file, runs `version` only on a Secure file, and caches 10 s by mtime/size.
  - Audit: `device.hook.started`, `device.hook.installed`, `device.hook.failed` with `reason`
    (`cancelled`, `timeout`, `failed`, `refused`); the result is recorded once when a check first shows it.
  - `device.pair` unsealed from an older panel gets "this panel is out of date; reload the page and try again".
- H2: done (`rubi-home open` in `internal/home/openwindow.go`; the panel's `#home-code=` handler in `panel/js/app.js`
  and the pure helpers in `panel/js/homecode.js`; T-01 pairing and fingerprint texts and allow-list lines removed).
  Review fixes: the link carries no secret (PIN design, `homeproto.Code.WithoutSecret`, `ParseCodeWithPin`, the optional
  `pin` arg of `device.pair`); five wrong secrets void a pending code; the fingerprint is shown in every dialog and in
  the panel; the code is made on demand and reused; `Helper.Update` takes a flock. Deviations:
  - Linux prints the link, the PIN and the code, with no allow-list entry (the text holds no command).
  - The panel's install steps still hold the `curl` line (T-01 install entries stay for H3; done in H3).
- H3: done (`scripts/build-home-app.sh`; `release.yml` builds the universal zip in a `mac-app` job and lists it in the sums; `internal/home/appinstall.go`
  and the `IsAppLaunch` branch in `cmd/rubi-home/main.go`; the panel's download screen). Details are in
  `docs/design/rubi-home.md` ("The Mac app"). Findings and deviations:
  - **Go linker signs ad hoc** on darwin/arm64 (`adhoc,linker-signed`, valid on disk). A plain copy of the binary
    verifies the same and carries no quarantine. The bundle as built does **not** verify (`Info.plist` is not bound:
    "code has no resources but signature indicates they must be present"); `codesign -s - --force` on it makes it
    valid. So the release builds the app on a macOS runner (job `mac-app`) and seals it ad hoc
    (`RUBI_APP_SIGN=adhoc`); the sealed zip is reproducible and verifies after unzipping
    (`TestHomeAppSealedBuildVerifies`). Whether macOS shows Open Anyway for it is for the H4 check on a test account.
  - The wait for the helper (10 s) dials its local socket (`claude.sock`); the window's helper is opened only after.
  - Reload rule: bootout and bootstrap for a new install, a changed LaunchAgent file or an agent that isn't loaded;
    `kickstart -k` when only the binary changed. A failed bootstrap enables the agent and tries once more.
  - Review fixes (H3 second pass):
    - No downgrade: a newer installed Rubi Home (read with `<installed> version`, 3 s) stays, with a one-button
      dialog; development builds are replaced only by development builds or when the version can't be read.
    - A loaded agent that does not answer gets one `kickstart -k` and a second wait.
    - The trust model is written down (HTTPS from GitHub plus Gatekeeper; nothing on the Mac checks `SHA256SUMS`).
    - The program that answers on the helper's socket is read from the kernel (`LOCAL_PEERPID` and the
      `kern.procargs2` sysctl, no cgo); a different one is logged, not refused.
    - Every launchctl call has a 20 s limit, dialogs 2 minutes, the pairing window 10 minutes.
    - `~/.rubi-home`, `bin/`, `logs/` must be real folders owned by the user; a symlink at the binary or the
      LaunchAgent file is replaced, never followed.
    - One universal app (`lipo` in the `mac-app` job): one download link, no Intel link.
    - `IsAppLaunch` also needs `Contents/Info.plist` with the bundle identifier; the home folder comes from
      `user.Current` (absolute); the dialog points to the log only when there is one.
    - The feed gains `mac_app` (`deploy/pages/latest.jq`), only when the release has the zip; without it the
      button opens the releases page.
    - The script writes the zip under a temporary name, shows codesign's output on failure, and cleans up on
      interrupt.
    - With `CGO_ENABLED=0` `os/user` reads `$HOME` on macOS; the absolute-path rule is the guard that remains.
  - `LSMinimumSystemVersion` is 13.0 (nothing else in the repo names a macOS version; Go 1.26 itself runs on 12).
  - `docs/user` and the README never told a Mac user to install Rubi Home with `curl` (their only `curl` is the
    Rubi install message, T-06), so no page needed rewriting.
  - The allow-list keeps two T-01L lines (the script line and "open a terminal and run:"), both inside "On Linux".
    The task pattern in `usertext_test.go` accepts `T-01L`.
  - Signing and notarizing: only a marked TODO in `release.yml`, no code.
- H4: not started.
