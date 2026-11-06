// container/rubi-claude-login against a recorded-shape fake of `claude auth login --claudeai` (no terminal, no network,
// a made-up credential).
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const helper = fileURLToPath(new URL("../../container/rubi-claude-login", import.meta.url));
const fake = fileURLToPath(new URL("../../test/support/fake-auth-login.mjs", import.meta.url));

interface Run {
  lines: string[];
  stdout: string;
  code: number | null;
  dir: string;
  extra: string;
  write(s: string): void;
  waitLine(pred: (l: string) => boolean): Promise<string>;
  done: Promise<void>;
  kill(): void;
}

// The shipped helper has claude's path and the grace period as constants, and takes nothing from the environment. A test
// runs a copy with those two lines changed, in a folder whose name has a space and a quote in it (nothing may be built
// into shell syntax), with a `script` first in the PATH that behaves like util-linux's `script -qfc CMD /dev/null` (the
// image's) and checks it is called that way. The real `script` runs in the live test in internal/home.
const hostile = "it's a dir";

function makeCopy(graceMs: number): { dir: string; helper: string; path: string } {
  const dir = mkdtempSync(join(tmpdir(), "rubi-login-")) + "/" + hostile;
  mkdirSync(join(dir, "bin"), { recursive: true });
  const wrapper = join(dir, "claude");
  writeFileSync(wrapper, '#!/bin/sh\nexec "$FAKE_NODE" "$FAKE_SCRIPT" "$@"\n', { mode: 0o755 });
  writeFileSync(
    join(dir, "bin", "script"),
    '#!/bin/sh\n[ "$1" = "-qfc" ] && [ "$3" = "/dev/null" ] && [ $# -eq 3 ] || { echo "script called wrongly: $*" >&2; exit 64; }\nexec /bin/sh -c "$2"\n',
    { mode: 0o755 },
  );
  let src = readFileSync(helper, "utf8");
  const before = src;
  src = src.replace(/^const CLAUDE = ".*";$/m, `const CLAUDE = ${JSON.stringify(wrapper)};`).replace(/^const GRACE_MS = \d+;/m, `const GRACE_MS = ${graceMs};`);
  assert.notEqual(src, before);
  assert.ok(src.includes(`const GRACE_MS = ${graceMs};`) && src.includes(JSON.stringify(wrapper)));
  const copy = join(dir, "rubi-claude-login");
  writeFileSync(copy, src, { mode: 0o755 });
  return { dir, helper: copy, path: join(dir, "bin") };
}

function run(env: Record<string, string | undefined> = {}, pre?: (dir: string) => void, graceMs = 1500): Run {
  const signin = mkdtempSync(join(tmpdir(), "rubi-signin-"));
  pre?.(signin);
  const copy = makeCopy(graceMs);
  const e: Record<string, string> = {
    PATH: `${copy.path}:${process.env.PATH ?? ""}`,
    CLAUDE_SECURESTORAGE_CONFIG_DIR: signin,
    FAKE_NODE: process.execPath,
    FAKE_SCRIPT: fake,
  };
  for (const [k, v] of Object.entries(env)) if (v === undefined) delete e[k]; else e[k] = v;
  const p = spawn(process.execPath, [copy.helper], { env: e, stdio: ["pipe", "pipe", "pipe"], detached: true });
  const r: Run = {
    lines: [],
    stdout: "",
    code: null,
    dir: signin,
    extra: copy.dir,
    write: (s) => p.stdin.write(s),
    kill: () => {
      try {
        process.kill(-p.pid!, "SIGKILL"); // the helper and the fake claude under it
      } catch {}
    },
    waitLine: (pred) =>
      new Promise((resolve, reject) => {
        const t = setTimeout(() => reject(new Error("no such line; saw " + JSON.stringify(r.lines))), 5000);
        const check = () => {
          const l = r.lines.find(pred);
          if (l !== undefined) {
            clearTimeout(t);
            resolve(l);
            return true;
          }
          return false;
        };
        if (check()) return;
        const iv = setInterval(() => check() && clearInterval(iv), 10);
        t.unref();
      }),
    done: new Promise((resolve) => p.on("close", (c) => ((r.code = c), resolve()))),
  };
  let buf = "";
  p.stdout.on("data", (d: Buffer) => {
    r.stdout += d.toString();
    buf += d.toString();
    let i;
    while ((i = buf.indexOf("\n")) >= 0) {
      r.lines.push(buf.slice(0, i));
      buf = buf.slice(i + 1);
    }
  });
  p.stderr.on("data", (d: Buffer) => (r.stdout += "[stderr]" + d.toString()));
  return r;
}

const clean = (r: Run) => {
  r.kill();
  try {
    chmodSync(join(r.dir, ".credentials.json"), 0o600);
  } catch {}
  rmSync(r.dir, { recursive: true, force: true });
  rmSync(join(r.extra, ".."), { recursive: true, force: true });
};
const noSecrets = (r: Run) => assert.ok(!/sk-ant|FAKEFAKE/i.test(r.stdout), "the credential leaked into the protocol: " + r.stdout);

describe("rubi-claude-login", () => {
  for (const gaps of ["0", "1", "2"]) {
    it(`link, prompt and code (${{ "0": "gaps drawn as spaces", "1": "gaps drawn as cursor moves", "2": "gaps drawn as moves and spaces" }[gaps]}), then SAVED`, async () => {
      const r = run({ FAKE_CURSOR_GAPS: gaps });
      try {
        const url = await r.waitLine((l) => l.startsWith("URL "));
        assert.match(url, /^URL https:\/\/claude\.com\/cai\/oauth\/authorize\?/);
        assert.ok(!url.includes("\u001b"), "escape characters in the link");
        await r.waitLine((l) => l === "CODE?");
        assert.ok(!existsSync(join(r.dir, ".credentials.json")), "a credential before the code");
        r.write("good\n");
        await r.done;
        assert.equal(r.lines.at(-1), "SAVED", r.stdout);
        assert.equal(r.code, 0);
        assert.ok(existsSync(join(r.dir, ".credentials.json")));
        assert.equal(r.lines.filter((l) => l.startsWith("URL ")).length, 1, "the link once");
        noSecrets(r);
      } finally {
        clean(r);
      }
    });
  }

  it("never opens the credential: it is enough that the file is there", async () => {
    const r = run({ FAKE_CRED_MODE: "000" });
    try {
      await r.waitLine((l) => l === "CODE?");
      r.write("good\n");
      await r.done;
      assert.equal(r.lines.at(-1), "SAVED", r.stdout);
      noSecrets(r);
    } finally {
      clean(r);
    }
  });

  it("a wrong code gets the prompt again, then a good one works", async () => {
    const r = run({ FAKE_BAD_CODE: "1", FAKE_CURSOR_GAPS: "1" });
    try {
      await r.waitLine((l) => l === "CODE?");
      r.write("wrong\n");
      await new Promise((res) => setTimeout(res, 100));
      assert.equal(r.lines.filter((l) => l === "CODE?").length, 2, r.stdout);
      r.write("good\n");
      await r.done;
      assert.equal(r.lines.at(-1), "SAVED", r.stdout);
    } finally {
      clean(r);
    }
  });

  it("a chunk is the code and at most one line end: anything else is refused and asked for again", async () => {
    const r = run({ FAKE_BAD_CODE: "1" });
    try {
      await r.waitLine((l) => l === "CODE?");
      let prompts = 1;
      for (const bad of ["good and more\n", "good\u0007\n", "good\nsecond\n", "good\r\n\r\n", "\n", "good\n\n"]) {
        r.write(bad);
        prompts++;
        await r.waitLine(() => r.lines.filter((l) => l === "CODE?").length === prompts);
        assert.ok(r.lines.some((l) => l.startsWith("FAILED the code had")), "no reason shown for " + JSON.stringify(bad));
        assert.equal(r.code, null, "the helper ended on a chunk it should have refused: " + JSON.stringify(bad));
        assert.ok(!existsSync(join(r.dir, ".credentials.json")));
      }
      // what is refused never reached claude: it saw no code, so it shows no "Invalid code" of its own
      assert.ok(!r.stdout.includes("Invalid"), r.stdout);
      r.write("good\r\n"); // one line end, whichever kind: accepted
      await r.done;
      assert.equal(r.lines.at(-1), "SAVED", r.stdout);
    } finally {
      clean(r);
    }
  });

  it("a code that arrives in pieces is one chunk", async () => {
    const r = run({ FAKE_BAD_CODE: "1" });
    try {
      await r.waitLine((l) => l === "CODE?");
      r.write("go");
      await new Promise((res) => setTimeout(res, 80));
      r.write("od\n");
      await r.done;
      assert.equal(r.lines.at(-1), "SAVED", r.stdout);
    } finally {
      clean(r);
    }
  });

  it("claude ending well without a credential is a failure", async () => {
    const r = run({ FAKE_NO_CODE: "1" });
    try {
      await r.done;
      assert.match(r.lines.at(-1)!, /^FAILED .*no credential/, r.stdout);
      assert.equal(r.code, 1);
    } finally {
      clean(r);
    }
  });

  it("a credential left by an earlier sign-in doesn't count", async () => {
    const r = run({ FAKE_NO_CODE: "1" }, (dir) => writeFileSync(join(dir, ".credentials.json"), "{}", { mode: 0o600 }));
    try {
      await r.done;
      assert.match(r.lines.at(-1)!, /^FAILED /, r.stdout);
    } finally {
      clean(r);
    }
  });

  it("SAVED waits for claude to end by itself; only after the grace a steady file is enough", async () => {
    const r = run({ FAKE_LINGER: "1" }, undefined, 2000);
    try {
      await r.waitLine((l) => l === "CODE?");
      const t0 = Date.now();
      r.write("good\n");
      await new Promise((res) => setTimeout(res, 900));
      assert.ok(existsSync(join(r.dir, ".credentials.json")));
      assert.ok(!r.lines.includes("SAVED"), "SAVED came before claude ended or the grace passed: " + r.stdout);
      await r.done;
      assert.equal(r.lines.at(-1), "SAVED", r.stdout);
      assert.ok(Date.now() - t0 >= 2000, `SAVED after ${Date.now() - t0} ms, before the grace`);
    } finally {
      clean(r);
    }
  });

  it("claude ending by itself gives SAVED at once, not after the grace", async () => {
    const r = run({}, undefined, 20000);
    try {
      await r.waitLine((l) => l === "CODE?");
      const t0 = Date.now();
      r.write("good\n");
      await r.done;
      assert.equal(r.lines.at(-1), "SAVED", r.stdout);
      assert.ok(Date.now() - t0 < 5000);
    } finally {
      clean(r);
    }
  });

  it("claude is started as `claude auth login --claudeai` through script -qfc, whatever the folder is called", async () => {
    // the fake claude refuses other arguments, the fake script refuses other calls, and the folder name has a quote and a space
    const r = run();
    try {
      assert.ok(r.extra.includes(hostile));
      await r.waitLine((l) => l.startsWith("URL "));
      assert.ok(!r.stdout.includes("unexpected arguments") && !r.stdout.includes("script called wrongly"), r.stdout);
    } finally {
      clean(r);
    }
  });

  it("the shipped helper takes nothing from the environment that chooses what it runs", () => {
    const src = readFileSync(helper, "utf8");
    assert.ok(!/RUBI_LOGIN_CMD|RUBI_CLAUDE_BIN|process\.env\.RUBI_/.test(src.replace(/RUBI_CLAUDE: CLAUDE/, "")), "an environment hook is back");
    assert.match(src, /^const CLAUDE = "\/opt\/rubi-runner\/.*";$/m);
    assert.ok(!/\$\{CLAUDE\}|`[^`]*\$\{[^}]*\}[^`]*auth login/.test(src), "claude's path is built into a shell string");
  });

  it("a link to another host is not shown", async () => {
    const r = run({ FAKE_LINK: "https://evil.example.com/cai/oauth/authorize?code=true" });
    try {
      await r.done;
      assert.equal(r.lines.at(-1), "FAILED unexpected sign-in link", r.stdout);
      assert.ok(!r.lines.some((l) => l.startsWith("URL ")));
      assert.equal(r.code, 1);
    } finally {
      clean(r);
    }
  });

  it("a link that carries something credential-shaped is not shown", async () => {
    const r = run({ FAKE_LINK: "https://claude.ai/cai/oauth/authorize?state=sk-ant-oat01-FAKEFAKE0123456789abcdefghijklmnopqrstuvwxyz" });
    try {
      await r.done;
      assert.equal(r.lines.at(-1), "FAILED unexpected sign-in link", r.stdout);
      noSecrets(r);
    } finally {
      clean(r);
    }
  });

  it("without a sign-in folder it fails at once", async () => {
    const r = run({ CLAUDE_SECURESTORAGE_CONFIG_DIR: undefined });
    try {
      await r.done;
      assert.match(r.lines.at(-1)!, /^FAILED no sign-in folder/);
    } finally {
      clean(r);
    }
  });
});
