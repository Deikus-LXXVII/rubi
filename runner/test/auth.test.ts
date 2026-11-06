// The runner never holds a Claude credential: Home gives it none (claude signs in with its own CLI, in T3 sessions). These
// tests cover the runner's defense in depth: anything shaped like a credential is redacted before it leaves, and it
// passes none of its environment on to claude beyond a short list.
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { redactCredentials } from "../src/protocol.js";
import { buildChildEnv } from "../src/runner.js";
import { Home } from "./harness.js";

// A made-up credential of the real shape; it is not a credential.
const FAKE = "sk-ant-oat01-FAKEFAKEFAKE0123456789_abcdefghij-KLMNOP";

const start = (h: Home) => h.ok("session.start", { prompt: "go", mode: "bypassPermissions" });

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

describe("the environment of claude", () => {
  it("passes on a short list and no token, whatever the runner was given", () => {
    const e = buildChildEnv({ PATH: "/usr/bin", HOME: "/h", CLAUDE_CODE_USE_BEDROCK: "1", ANTHROPIC_AUTH_TOKEN: "y", AWS_SECRET_ACCESS_KEY: "z", HTTPS_PROXY: "http://p" }, "/cfg");
    assert.equal(e.CLAUDE_CODE_USE_BEDROCK, undefined);
    assert.equal(e.ANTHROPIC_AUTH_TOKEN, undefined);
    assert.equal(e.AWS_SECRET_ACCESS_KEY, undefined);
    assert.equal(e.CLAUDE_CONFIG_DIR, "/cfg");
    assert.equal(e.HTTPS_PROXY, "http://p");
  });
});
