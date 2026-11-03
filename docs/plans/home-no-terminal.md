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
  `display dialog`, which needs no new dependency).
  - **Not paired:** the dialog offers **Copy the pairing code** and **Open the Rubi panel**.
    - Copy puts the code on the clipboard, so Universal Clipboard can carry it to a phone.
  - **Paired:** the dialog shows which Rubi it is paired with and the fingerprint. It offers **Pair again**, which makes
    a new code and goes back to the case above.
  - The code is the one `rubi-home pair` makes today: one time, 15 minutes.
- **Panel link.**
  - Open the Rubi panel opens `https://rubi-panel.com/#home-code=<code>`.
  - On load, the panel reads the code, removes it from the address bar at once, and keeps it in local storage for at
    most 15 minutes.
  - It opens the "Add a home computer" screen with the code filled in, once the user opens Rubi with their own link.
  - Pair still needs the passkey approval, as today.
  - The code is deleted after use or expiry.
  - `device.pair` becomes a sensitive op (its args hold the code's secret).
- **Texts.**
  - "rubi-home status shows the same" becomes "Rubi Home shows the same fingerprint when you open it".
  - "start it with: rubi-home run" and "run `rubi-home pair` again" become "open Rubi Home".
- **Checks.**
  - Dialog code with a fake runner, covering the not-paired, paired and Pair-again cases.
  - The panel link test: the code leaves the URL, expires, and is used once.
  - Panel flow test.

## H3: `Rubi Home.app` (T-01)

- **The release** builds `rubi-home-<version>-macos-<arch>.zip` for arm64 and amd64.
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
- **The panel's "Add a home computer" screen.** On a Mac or a phone, it shows a **Download Rubi Home for Mac** link to
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
- H2, H3, H4: not started.
