// The sign-in shared by all projects: only the token is shared, read at each query start, into the SDK
// child's environment and nowhere else.
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, it } from "node:test";
import { readSharedToken } from "../src/runner.js";
import { Home, TOKEN as RUN_TOKEN } from "./harness.js";

// A made-up token of the real shape; it is not a credential.
const TOKEN = "sk-ant-oat01-FAKEFAKEFAKE0123456789_abcdefghij-KLMNOP";

function authDir(token?: string): string {
  const dir = mkdtempSync(join(tmpdir(), "rubi-auth-"));
  mkdirSync(join(dir, "token"), { recursive: true });
  if (token !== undefined) writeFileSync(join(dir, "token", "oauth-token"), token + "\n", { mode: 0o600 });
  return dir;
}

const start = (h: Home) => h.ok("session.start", { prompt: "go", mode: "bypassPermissions" });

describe("shared sign-in token", () => {
  it("is read at each query start and goes only into the child's env", async () => {
    const dir = authDir();
    try {
      const h = await new Home({ authDir: dir }).up();
      h.sdk.turn = (ctx) => ctx.result();
      await start(h);
      await h.waitEvent("result");
      assert.equal(h.sdk.queries[0]!.options.env!.CLAUDE_CODE_OAUTH_TOKEN, undefined); // nothing yet
      writeFileSync(join(dir, "token", "oauth-token"), TOKEN + "\n");
      await start(h);
      await h.waitEvent("result", () => true, h.lines.length - 1);
      const env = h.sdk.queries[1]!.options.env!;
      assert.equal(env.CLAUDE_CODE_OAUTH_TOKEN, TOKEN);
      // the project's own config dir is untouched: only the token is shared
      assert.equal(env.CLAUDE_CONFIG_DIR, "/work/.rubi/claude");
      // never on the wire (events, answers) or in a log
      const everything = JSON.stringify(h.lines) + h.logs.join("\n");
      assert.equal(everything.includes("FAKEFAKE"), false);
      // removing the token (sign-out) applies to the next query
      rmSync(join(dir, "token", "oauth-token"));
      await start(h);
      await h.waitFor(() => h.sdk.queries.length === 3);
      assert.equal(h.sdk.queries[2]!.options.env!.CLAUDE_CODE_OAUTH_TOKEN, undefined);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("the usage probe uses it too", async () => {
    const dir = authDir(TOKEN);
    try {
      const h = await new Home({ authDir: dir }).up();
      h.sdk.probeResult = { account: { subscriptionType: "max", tokenSource: "CLAUDE_CODE_OAUTH_TOKEN" } };
      const r = (await h.ok("usage.probe")) as Record<string, any>;
      assert.equal(h.sdk.probeEnv!.CLAUDE_CODE_OAUTH_TOKEN, TOKEN);
      assert.equal(r.logged_in, true);
      assert.equal(JSON.stringify(r).includes("FAKEFAKE"), false);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("RUBI_AUTH_DIR names the directory; the Read tool is denied there", async () => {
    const dir = authDir(TOKEN);
    try {
      const h = await new Home({ env: { PATH: "/usr/bin", RUBI_RUN_TOKEN: RUN_TOKEN, RUBI_AUTH_DIR: dir } }).up();
      h.sdk.turn = (ctx) => ctx.result();
      await start(h);
      await h.waitEvent("result");
      const q = h.sdk.queries[0]!.options;
      assert.equal(q.env!.CLAUDE_CODE_OAUTH_TOKEN, TOKEN);
      assert.ok(q.disallowedTools!.includes(`Read(/${dir}/**)`), String(q.disallowedTools));
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("claude's own stderr never puts a token into the log", async () => {
    const dir = authDir(TOKEN);
    try {
      const h = await new Home({ authDir: dir }).up();
      h.sdk.turn = (ctx) => {
        ctx.q.options.stderr?.(`oops ${TOKEN} failed\n`);
        ctx.result();
      };
      await start(h);
      await h.waitEvent("result");
      assert.ok(h.logs.some((l) => l.includes("[redacted]")));
      assert.equal(h.logs.join("\n").includes("FAKEFAKE"), false);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("readSharedToken: missing, empty, spaced and oversized files are no token", () => {
    const none = mkdtempSync(join(tmpdir(), "rubi-auth-"));
    try {
      assert.equal(readSharedToken(none), undefined);
      for (const bad of ["", "\n", "two words", "a".repeat(5000), "tab\there"]) {
        const d = authDir(bad);
        assert.equal(readSharedToken(d), undefined, JSON.stringify(bad.slice(0, 20)));
        rmSync(d, { recursive: true, force: true });
      }
      const ok = authDir(TOKEN);
      assert.equal(readSharedToken(ok), TOKEN);
      rmSync(ok, { recursive: true, force: true });
    } finally {
      rmSync(none, { recursive: true, force: true });
    }
  });
});
