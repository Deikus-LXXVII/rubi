// The runner never holds the Claude token. Home gives it the address of its auth gateway as the base URL and a
// per-start secret as the OAuth token; the gateway holds the real token. These tests cover what the runner does
// with that, and its defense in depth: anything shaped like a credential is redacted before it leaves.
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { redactCredentials } from "../src/protocol.js";
import { buildChildEnv } from "../src/runner.js";
import { Home, TOKEN as RUN_TOKEN } from "./harness.js";

// A made-up credential of the real shape; it is not a credential.
const FAKE = "sk-ant-oat01-FAKEFAKEFAKE0123456789_abcdefghij-KLMNOP";
// The per-start secret Home hands out (random hex, not credential-shaped).
const SECRET = "ab12".repeat(16);
const GATEWAY = "http://192.168.64.1:49152";

const start = (h: Home) => h.ok("session.start", { prompt: "go", mode: "bypassPermissions" });
const env = (extra: Record<string, string> = {}) => ({ PATH: "/usr/bin", RUBI_RUN_TOKEN: RUN_TOKEN, ...extra });

describe("the gateway, from the runner's side", () => {
  it("the SDK child gets the base URL and the per-start secret, and the CONNECT proxy is bypassed for the gateway", async () => {
    const h = await new Home({
      env: env({ ANTHROPIC_BASE_URL: GATEWAY, CLAUDE_CODE_OAUTH_TOKEN: SECRET, NO_PROXY: "192.168.64.1", HTTPS_PROXY: "http://rubi:x@192.168.64.1:3128" }),
    }).up();
    h.sdk.turn = (ctx) => ctx.result();
    await start(h);
    await h.waitEvent("result");
    const e = h.sdk.queries[0]!.options.env!;
    assert.equal(e.ANTHROPIC_BASE_URL, GATEWAY);
    assert.equal(e.CLAUDE_CODE_OAUTH_TOKEN, SECRET);
    assert.equal(e.NO_PROXY, "192.168.64.1");
    assert.equal(e.CLAUDE_CONFIG_DIR, "/work/.rubi/claude"); // the project's own config dir, as before
    // the secret never goes on the wire to Home (no event, answer or question) nor into a log
    assert.equal(JSON.stringify(h.lines).includes(SECRET), false);
    assert.equal(h.logs.join("\n").includes(SECRET), false);
  });

  it("an API key in the environment is not passed along with the OAuth token (Claude Code prefers the key)", () => {
    const withToken = buildChildEnv({ ANTHROPIC_API_KEY: FAKE, CLAUDE_CODE_OAUTH_TOKEN: SECRET, ANTHROPIC_BASE_URL: GATEWAY }, "/work/.rubi/claude");
    assert.equal(withToken.ANTHROPIC_API_KEY, undefined);
    assert.equal(withToken.CLAUDE_CODE_OAUTH_TOKEN, SECRET);
    const without = buildChildEnv({ ANTHROPIC_API_KEY: "mock-key" }, "/work/.rubi/claude");
    assert.equal(without.ANTHROPIC_API_KEY, "mock-key");
    assert.equal(without.CLAUDE_CODE_OAUTH_TOKEN, undefined);
  });

  it("the runner reads no token file: whatever lies in an old sign-in directory is ignored", async () => {
    const h = await new Home({ env: env({ RUBI_AUTH_DIR: "/nonexistent" }) }).up();
    h.sdk.turn = (ctx) => ctx.result();
    await start(h);
    await h.waitEvent("result");
    assert.equal(h.sdk.queries[0]!.options.env!.CLAUDE_CODE_OAUTH_TOKEN, undefined);
  });

  it("the usage probe is given the same environment", async () => {
    const h = await new Home({ env: env({ ANTHROPIC_BASE_URL: GATEWAY, CLAUDE_CODE_OAUTH_TOKEN: SECRET }) }).up();
    h.sdk.probeResult = { account: { subscriptionType: "max", tokenSource: "CLAUDE_CODE_OAUTH_TOKEN" } };
    const r = (await h.ok("usage.probe")) as Record<string, any>;
    assert.equal(h.sdk.probeEnv!.ANTHROPIC_BASE_URL, GATEWAY);
    assert.equal(r.logged_in, true);
  });
});

describe("credentials never leave the runner", () => {
  it("claude's own stderr never puts a credential into the log", async () => {
    const h = await new Home().up();
    h.sdk.turn = (ctx) => {
      ctx.q.options.stderr?.(`oops ${FAKE} failed\n`);
      ctx.result();
    };
    await start(h);
    await h.waitEvent("result");
    assert.ok(h.logs.some((l) => l.includes("[redacted]")));
    assert.equal(h.logs.join("\n").includes("FAKEFAKE"), false);
  });

  it("anything shaped like a credential is redacted in every event, answer and question before it leaves", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      ctx.say(`the key is ${FAKE} ok`);
      await ctx.tool("Bash", { command: `echo ${FAKE}` });
      ctx.result({ result: `done ${FAKE}` });
    };
    await start(h);
    await h.waitEvent("result");
    const everything = JSON.stringify(h.lines);
    assert.equal(everything.includes("FAKEFAKE"), false, everything);
    assert.ok(everything.includes("[redacted credential]"));
    assert.equal(redactCredentials("sk-ant-short"), "sk-ant-short"); // too short to be one
    assert.equal(redactCredentials(`a ${FAKE} b ${FAKE}`), "a [redacted credential] b [redacted credential]");
  });
});
