import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { EXIT_HELLO_FAILED, EXIT_NO_TOKEN, buildChildEnv, disallowedEditTools } from "../src/runner.js";
import { TOKEN, Home, tick } from "./harness.js";

describe("handshake", () => {
  it("waits 10 s for hello by default", () => {
    assert.equal(new Home().runner["cfg"].helloTimeoutMs, 10_000);
  });

  it("exits at once without RUBI_RUN_TOKEN and sends nothing", async () => {
    const h = new Home({ env: { RUBI_RUN_TOKEN: undefined } });
    await h.runner.start();
    assert.deepEqual(h.exits, [EXIT_NO_TOKEN]);
    assert.equal(h.lines.length, 0);
  });

  it("sends hello {run_token, version} first and is ready after {ok:true}", async () => {
    const h = await new Home().up();
    const hello = h.lines[0]!;
    assert.equal(hello.msg.method, "hello");
    assert.deepEqual(hello.msg.params, { run_token: TOKEN, version: "test" });
    assert.deepEqual(h.exits, []);
    assert.deepEqual(await h.ok("session.list"), { sessions: [] });
  });

  it("exits when Home refuses (wrong token: stdin closed)", async () => {
    const h = new Home({ env: { RUBI_RUN_TOKEN: "wrong" } });
    await h.runner.start();
    assert.deepEqual(h.exits, [EXIT_HELLO_FAILED]);
  });

  it("exits when Home answers hello with an error or ok:false", async () => {
    const h = new Home();
    h.helloHandler = () => ({ ok: false });
    await h.runner.start();
    assert.deepEqual(h.exits, [EXIT_HELLO_FAILED]);
  });

  it("exits when there is no answer to hello within the time limit", async () => {
    const h = new Home({ helloTimeoutMs: 80 });
    const orig = h.reply.bind(h);
    h.reply = () => {}; // swallow the answer: never answered, never closed
    await h.runner.start();
    h.reply = orig;
    assert.deepEqual(h.exits, [EXIT_HELLO_FAILED]);
  });

  it("refuses every request before the handshake is done", async () => {
    const h = new Home({ helloTimeoutMs: 300 });
    const orig = h.reply.bind(h);
    h.reply = () => {}; // hello stays unanswered
    const started = h.runner.start();
    await h.waitFor((l) => l.msg.method === "hello");
    const r = await h.call("session.list");
    assert.equal(r.error.code, -32001);
    h.reply = orig;
    await started;
  });

  it("never hands the token to the claude child", () => {
    const env = buildChildEnv({ PATH: "/bin", RUBI_RUN_TOKEN: TOKEN, SECRET_THING: "x", HTTPS_PROXY: "http://p:1" }, "/work/.rubi/claude");
    assert.equal(env.RUBI_RUN_TOKEN, undefined);
    assert.equal(env.SECRET_THING, undefined);
    assert.equal(env.CLAUDE_CONFIG_DIR, "/work/.rubi/claude");
    assert.equal(env.DISABLE_TELEMETRY, "1");
    assert.equal(env.DISABLE_ERROR_REPORTING, "1");
    assert.equal(env.CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC, "1");
    assert.equal(env.HTTPS_PROXY, "http://p:1");
  });

  it("removes the token from its own environment", async () => {
    const env: NodeJS.ProcessEnv = { RUBI_RUN_TOKEN: TOKEN };
    const h = new Home({ env });
    await h.up();
    assert.equal(h.runner["env"].RUBI_RUN_TOKEN, undefined);
  });
});

describe("protocol robustness", () => {
  it("answers a malformed line with a parse error (id null) and keeps going", async () => {
    const h = await new Home().up();
    h.writeBytes("this is not json\n");
    const e = await h.waitFor((l) => l.from === "runner" && l.msg.error?.code === -32700);
    assert.equal(e.msg.id, null);
    assert.deepEqual(await h.ok("session.list"), { sessions: [] });
  });

  it("answers JSON that is not a request, arrays and a wrong jsonrpc version", async () => {
    const h = await new Home().up();
    h.writeBytes("[1,2]\n");
    h.writeBytes('{"jsonrpc":"1.0","id":7,"method":"session.list"}\n');
    h.writeBytes('{"jsonrpc":"2.0","id":8}\n');
    await h.waitFor((l) => l.msg.id === 7 && l.msg.error?.code === -32600);
    await h.waitFor((l) => l.msg.id === 8 && l.msg.error?.code === -32600);
    assert.ok(h.lines.filter((l) => l.from === "runner" && l.msg.error?.code === -32600).length >= 3);
    assert.deepEqual(await h.ok("session.list"), { sessions: [] });
  });

  it("answers an unknown method and bad params with errors that carry the id", async () => {
    const h = await new Home().up();
    assert.equal((await h.call("nope")).error.code, -32601);
    assert.equal((await h.call("session.send", { id: 5 })).error.code, -32602);
    assert.equal((await h.call("session.start", { prompt: "x", mode: "yolo" })).error.code, -32602);
    assert.equal((await h.call("session.interrupt", { id: "nope" })).error.code, -32004);
  });

  it("skips a line over the limit with an error and reads the next line", async () => {
    const h = await new Home({ maxLineBytes: 1024 }).up();
    h.writeBytes('{"jsonrpc":"2.0","id":1,"method":"session.list","params":{"pad":"' + "a".repeat(5000) + '"}}\n');
    const e = await h.waitFor((l) => l.from === "runner" && l.msg.error?.code === -32005);
    assert.equal(e.msg.id, null);
    assert.deepEqual(await h.ok("session.list"), { sessions: [] });
  });

  it("handles a line far over the limit arriving in many chunks, and a big line under the limit", async () => {
    const h = await new Home({ maxLineBytes: 64 * 1024 }).up();
    for (let i = 0; i < 40; i++) h.writeBytes("b".repeat(32 * 1024)); // 1.25 MB, no newline yet
    h.writeBytes("\n");
    await h.waitFor((l) => l.from === "runner" && l.msg.error?.code === -32005);
    const big = "x".repeat(40 * 1024);
    const r = await h.call("session.start", { prompt: big, mode: "default" });
    assert.ok(r.result.id);
    await h.waitEvent("assistant", (e) => e.text.length > 40 * 1024);
  });

  it("does not split a multi-byte character across chunks", async () => {
    const h = await new Home().up();
    const line = Buffer.from(JSON.stringify({ jsonrpc: "2.0", id: 99, method: "session.start", params: { prompt: "héllo \u{1F600}", mode: "default" } }) + "\n");
    const cut = line.indexOf(0xc3) + 1;
    h.writeBytes(line.subarray(0, cut));
    await tick();
    h.writeBytes(line.subarray(cut));
    await h.waitFor((l) => l.from === "runner" && l.msg.id === 99);
    assert.equal(h.sdk.last.received[0]!.text, "héllo \u{1F600}");
  });

  it("shuts the sessions down and exits 0 when stdin ends", async () => {
    const h = await new Home().up();
    await h.ok("session.start", { prompt: "hi", mode: "default" });
    h.closeStdin();
    await tick(30);
    assert.deepEqual(h.exits, [0]);
    assert.equal(h.sdk.last.closed, true);
  });
});

describe("options of every query", () => {
  it("sets cwd, project settings, bypass allowed, the mode from Home, env, disallowed tools, hook timeout 300", async () => {
    const h = await new Home().up();
    const { id } = await h.ok("session.start", { prompt: "hi", mode: "acceptEdits" });
    const o = h.sdk.last.options;
    assert.equal(o.cwd, "/work");
    assert.equal(o.sessionId, id);
    assert.equal(o.resume, undefined);
    assert.deepEqual(o.settingSources, ["project"]);
    assert.equal(o.allowDangerouslySkipPermissions, true);
    assert.equal(o.permissionMode, "acceptEdits");
    assert.equal(o.env.CLAUDE_CONFIG_DIR, "/work/.rubi/claude");
    assert.equal(o.env.HTTPS_PROXY, "http://proxy.invalid:3128");
    assert.equal(o.env.DISABLE_TELEMETRY, "1");
    assert.equal(o.hooks.PreToolUse[0]!.timeout, 300);
    assert.deepEqual(o.disallowedTools, disallowedEditTools("/work"));
    assert.ok(o.disallowedTools.includes("Write(//work/.rubi/**)"));
    assert.ok(o.disallowedTools.includes("Edit(//work/.rubi/**)"));
    assert.equal(typeof o.canUseTool, "function");
  });

  it("passes name as the title so the SDK skips title generation", async () => {
    const h = await new Home().up();
    await h.ok("session.start", { prompt: "hi", name: "my name", mode: "default" });
    assert.equal(h.sdk.last.options.title, "my name");
  });
});
