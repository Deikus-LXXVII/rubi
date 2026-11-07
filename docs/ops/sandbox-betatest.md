# Sandbox and beta test

A disposable Rubi for trying a change before it reaches a live install. It builds the daemon and the
panel from this checkout, generates its own signing keys, and runs inside a rootless user, network
and PID namespace. It does not read `~/.rubi`, `~/.rubi-home`, or any signing key already in the
environment.

The machine needs Linux, `unshare`, Go, Node, and `python3`. It does not need Docker, Podman, or root.
Playwright is installed into the sandbox by the beta-test script.

## Sandbox

```bash
scripts/sandbox/up.sh
scripts/sandbox/down.sh
scripts/sandbox/reset.sh
```

`up` is safe to repeat: if that sandbox is already running, it leaves it. `reset` deletes only a
directory that carries the sandbox marker.

`RUBI_SANDBOX_DIR` chooses the directory (the default is `.sandbox` in this repository). The panel is
at `http://127.0.0.1:18080` inside the namespace. From the host, reach it with:

```bash
scripts/sandbox/run.sh curl -fsS http://127.0.0.1:18080/
```

The fake Rubi Home advertises `t3-setup`. The sign-in step points at a local page that shows a test
code. Neither the code nor the pairing code is printed.

Plugins are installed from a catalog signed with the sandbox release key. The script builds
[T3 Agents](https://github.com/Deikus-LXXVII/rubi-t3agents) with the sandbox publisher key when that
build succeeds. Otherwise it uses the stand-in in `scripts/sandbox/plugin`, and the beta test marks
the T3 flows blocked.

`make sandbox-check` is the same non-UI check CI runs: up, the panel and catalog answer, the daemon
is unpaired in its own network namespace, down, up, reset, and a Rubi that was already running is
unchanged. The comparison ignores fields that move on their own, including how many relays are
connected.

## Beta test

```bash
scripts/sandbox/betatest.sh
```

This installs Playwright Chromium and WebKit under the sandbox directory (or the supplied
`PLAYWRIGHT_BROWSERS_PATH`) and drives the panel at 393×852 (touch) and 1280×800.
The host must provide the OS dependencies for both browsers; on a test runner, install them with
`npx playwright install-deps chromium webkit` from `scripts/betatest`. A missing browser fails the
run rather than silently skipping Safari coverage. Screenshots, `summary.json` and `summary.md`
are written to `/workspace/betatest/<run>`
when that directory can be created, otherwise to `<sandbox>/betatest/<run>`.

The run pairs with a password, pairs the fake Home, connects a Bot to a local webhook stub (so the
routine test succeeds), installs T3 Agents, opens an approval link and then the plugin page, opens a
`setup:t3agents` link, walks setup with one primary action, completes the sandbox sign-in (the
self-test runs on its own), and ends on Ready. In both engines and both viewport sizes it opens
the OAuth page in a new tab, returns, waits through two unchanged refresh periods, and checks that
Paste the code still opens an editable prompt, Escape closes it, and it can be opened again.

Screenshots wait for the finite entrance animation, then capture with animations disabled. The
mascot's idle animation never ends, so it is not waited on. A shot that is one flat colour fails.
`summary.md` lists every check with its own pass or fail: overflow, tap targets, error pages, the
sign-in link, and each screenshot.

## CI

The `sandbox` job runs `scripts/sandbox/check.sh` on every pull request. The browser run is the
`betatest` workflow, started by hand.
