// Behaviour added after the review of M1.
import assert from "node:assert/strict";
import { Writable } from "node:stream";
import { describe, it } from "node:test";
import { PassThrough } from "node:stream";
import { Peer } from "../src/protocol.js";
import { Home, tick } from "./harness.js";

const start = (h: Home, extra: Record<string, unknown> = {}) => h.ok("session.start", { prompt: "go", mode: "bypassPermissions", ...extra });
const Z = "00000000-0000-4000-8000-000000000000";

describe("permission callbacks never crash into an allow", () => {
  it("a throwing ask handler path ends in a deny from the hook", async () => {
    const h = await new Home().up();
    // make the peer fail in an unexpected way: perm.ask answered with something that breaks parsing
    h.askHandler = () => null as never;
    let outcome: any;
    h.sdk.turn = async (ctx) => {
      outcome = await ctx.tool("Bash", { command: "x" });
      ctx.result();
    };
    await start(h);
    await h.waitEvent("result");
    assert.equal(outcome.allowed, false);
  });

  it("an exception inside the hook is a deny", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      (h.runner as any).askHome = () => {
        throw new Error("bug");
      };
      const hook = ctx.q.options.hooks.PreToolUse[0]!.hooks[0]!;
      const out: any = await hook({ hook_event_name: "PreToolUse", session_id: "s", cwd: "/work", tool_name: "Bash", tool_input: {}, tool_use_id: "t" }, "t", { signal: new AbortController().signal });
      assert.equal(out.hookSpecificOutput.permissionDecision, "deny");
      const r = await ctx.q.options.canUseTool("Bash", {}, { signal: new AbortController().signal, toolUseID: "t2" });
      assert.equal(r.behavior, "deny");
      ctx.result();
    };
    await start(h);
    await h.waitEvent("result");
  });

  it("the remembered decision is matched by tool name and input, not by id alone", async () => {
    const h = await new Home().up();
    let second: unknown;
    h.sdk.turn = async (ctx) => {
      const r = await ctx.tool("Bash", { command: "ls" });
      second = await ctx.canUseTool("Bash", { command: "rm -rf /" }, r.id); // same id, different input
      ctx.result();
    };
    await start(h, { mode: "default" });
    await h.waitEvent("result");
    assert.equal(h.asks.length, 2);
    assert.equal(h.asks[1]!.via, "can_use_tool");
  });

  it("a remembered decision is used once", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      const r = await ctx.tool("Bash", { command: "ls" });
      await ctx.canUseTool("Bash", { command: "ls" }, r.id);
      await ctx.canUseTool("Bash", { command: "ls" }, r.id);
      ctx.result();
    };
    await start(h, { mode: "default" });
    await h.waitEvent("result");
    assert.equal(h.asks.length, 2);
  });
});

describe("held-back messages", () => {
  it("a query that dies flushes held-back messages, in order, into a resumed query", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      if (ctx.text === "first") {
        await ctx.hold();
        ctx.q.out.end(); // the process dies mid-turn
        return;
      }
      ctx.result();
    };
    const { id } = await start(h, { prompt: "first" });
    await h.ok("session.send", { id, text: "second" });
    await h.ok("session.send", { id, text: "third" });
    h.sdk.queries[0]!.release();
    await h.waitFor(() => h.events("result").length === 2);
    assert.equal(h.sdk.queries.length, 2);
    assert.equal(h.sdk.queries[1]!.options.resume, id);
    assert.deepEqual(h.sdk.queries[1]!.received.map((r) => r.text), ["second", "third"]);
  });

  it("the held-back queue is bounded; the overflow is an error and an event", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      await ctx.hold();
    };
    const { id } = await start(h);
    for (let i = 0; i < 50; i++) await h.ok("session.send", { id, text: `m${i}` });
    const r = await h.call("session.send", { id, text: "one too many" });
    assert.equal(r.error.code, -32006);
    assert.match((await h.waitEvent("error")).message, /queue full/);
    h.sdk.last.release();
  });

  it("a crash loop ends: held-back messages are dropped with an error after a few respawns", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      if (ctx.text === "first") await ctx.hold();
      ctx.q.out.end(); // every query dies without a result
    };
    const { id } = await start(h, { prompt: "first" });
    for (let i = 0; i < 6; i++) await h.ok("session.send", { id, text: `m${i}` });
    h.sdk.last.release();
    await h.waitEvent("error", (e) => /keeps ending/.test(e.message));
    assert.ok(h.sdk.queries.length <= 5);
  });

  it("a turn the SDK started itself holds back new messages until its result", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      if (ctx.text === "go") ctx.result();
    };
    const { id } = await start(h);
    await h.waitFor(() => h.statuses().at(-1) === "idle");
    h.sdk.last.emit({ type: "assistant", parent_tool_use_id: null, message: { content: [{ type: "text", text: "a background task finished" }] } });
    await h.waitFor(() => h.statuses().at(-1) === "busy");
    const r = await h.ok("session.send", { id, text: "meanwhile" });
    assert.equal(r.delivered, false);
    h.sdk.last.emit({ type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: id });
    await tick(30);
    assert.ok(h.sdk.last.received.some((x) => x.text === "meanwhile"));
  });

  it("a subagent's own messages do not count as a turn of the main session", async () => {
    const h = await new Home().up();
    h.sdk.turn = (ctx) => ctx.result();
    await start(h);
    await h.waitFor(() => h.statuses().at(-1) === "idle");
    h.sdk.last.emit({ type: "assistant", parent_tool_use_id: "toolu_x", message: { content: [{ type: "text", text: "sub" }] } });
    await tick(20);
    assert.equal(h.statuses().at(-1), "idle");
  });

  it("interrupt drops held-back messages", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      await ctx.hold();
      ctx.result();
    };
    const { id } = await start(h);
    await h.ok("session.send", { id, text: "held" });
    await h.ok("session.interrupt", { id });
    h.sdk.last.release();
    await h.waitEvent("result");
    await tick(20);
    assert.deepEqual(h.sdk.last.received.map((r) => r.text), ["go"]);
  });
});

describe("start and send failures leave nothing behind", () => {
  it("session.start rolls back when the SDK throws synchronously", async () => {
    const h = await new Home().up();
    h.sdk.query = () => {
      throw new Error("cannot spawn claude");
    };
    assert.equal((await h.call("session.start", { prompt: "x", mode: "default" })).error.code, -32603);
    const l = (await h.ok("session.list")) as { sessions: unknown[] };
    assert.deepEqual(l.sessions, []);
  });

  it("session.send to an unknown session does not leave a registration when opening fails", async () => {
    const h = await new Home().up();
    h.sdk.query = () => {
      throw new Error("cannot spawn claude");
    };
    assert.equal((await h.call("session.send", { id: Z, text: "x" })).error.code, -32603);
    const l = (await h.ok("session.list")) as { sessions: unknown[] };
    assert.deepEqual(l.sessions, []);
  });
});

describe("a slow reader", () => {
  it("never blocks the runner; the oldest assistant events are dropped with one notice, responses are kept", async () => {
    const chunks: string[] = [];
    let release: (() => void) | null = null;
    const slow = new Writable({
      highWaterMark: 1,
      write(chunk, _enc, cb) {
        chunks.push(chunk.toString());
        release = cb; // hold the callback: the reader is "stuck"
      },
    });
    const input = new PassThrough();
    const peer = new Peer(input, slow, { onRequest: async () => ({ ok: true }), maxQueuedBytes: 2000 });
    peer.notify("event", { session_id: "s1", kind: "status", status: "busy" }); // first write blocks
    for (let i = 0; i < 200; i++) peer.notify("event", { session_id: "s1", kind: "assistant", text: "x".repeat(50), i }, "s1");
    input.write(JSON.stringify({ jsonrpc: "2.0", id: 1, method: "m", params: {} }) + "\n"); // a response must survive
    await tick(20);
    while (release) {
      const r: () => void = release;
      release = null;
      r();
      await tick(2);
    }
    const lines = chunks.join("").split("\n").filter(Boolean).map((l) => JSON.parse(l));
    const assistants = lines.filter((l) => l.params?.kind === "assistant");
    assert.ok(assistants.length < 200 && assistants.length > 0);
    assert.equal(assistants.at(-1).params.i, 199, "the newest is kept");
    const notice = lines.find((l) => l.params?.kind === "error");
    assert.equal(notice.params.dropped + assistants.length, 200);
    assert.ok(lines.some((l) => l.id === 1 && l.result?.ok === true));
  });
});
