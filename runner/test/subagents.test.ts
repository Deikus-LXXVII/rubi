import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { Home, tick } from "./harness.js";

const start = (h: Home, cap: number | undefined, extra: Record<string, unknown> = {}) =>
  h.ok("session.start", { prompt: "go", mode: "bypassPermissions", ...(cap !== undefined ? { max_subagents: cap } : {}), ...extra });
// The runner reads the fake's messages asynchronously: a test waits a moment after the events it needs seen.
const task = (q: { emit(m: any): void }, id: string, ended = false) =>
  q.emit(ended ? { type: "system", subtype: "task_notification", task_id: id } : { type: "system", subtype: "task_started", task_id: id, is_backgrounded: true });
const agentCall = { subagent_type: "general-purpose", description: "look around" };

describe("the subagent cap", () => {
  it("a subagent call waits (is not denied) while the session has its cap running, and goes on when one ends", async () => {
    const h = await new Home().up();
    let outcome = undefined as any;
    const seen = () => outcome;
    h.sdk.turn = async (ctx) => {
      task(ctx, "t1");
      task(ctx, "t2");
      await tick(20);
      outcome = await ctx.tool("Agent", agentCall);
      ctx.result();
    };
    await start(h, 2);
    const w = await h.waitEvent("waiting", (p) => !p.done);
    assert.match(w.reason, /^waiting: this session already has 2 subagents running, the limit$/);
    await tick(50);
    assert.equal(h.asks.length, 1, "asked at once: the ask does not wait for the gate");
    assert.equal(seen(), undefined, "but the call does not go on");
    task(h.sdk.last, "t1", true);
    await h.waitEvent("waiting", (p) => p.done === true);
    await h.waitEvent("result");
    assert.equal(seen()?.allowed, true);
    assert.equal(h.asks.length, 1);
    assert.equal(h.asks[0]!.tool, "Agent");
    assert.deepEqual(h.events("subagents").map((e) => e.count).slice(0, 4), [1, 2, 1, 2]);
  });

  it("foreground calls count until their result, and a Task call is gated like an Agent call", async () => {
    const h = await new Home().up();
    let release!: () => void;
    const held = new Promise<void>((r) => (release = r));
    const done: string[] = [];
    h.sdk.turn = async (ctx) => {
      const a = ctx.tool("Agent", agentCall, { holdResult: held }).then(() => done.push("first"));
      await tick(20);
      const b = ctx.tool("Task", agentCall).then(() => done.push("second"));
      await Promise.all([a, b]);
      ctx.result();
    };
    await start(h, 1);
    await h.waitEvent("waiting");
    await tick(50);
    assert.deepEqual(done, []);
    release();
    await h.waitEvent("result");
    assert.deepEqual(done, ["first", "second"]);
    assert.equal(h.events("subagents").at(-1)!.count, 0);
  });

  it("no cap (0 or absent) never waits", async () => {
    for (const cap of [0, undefined]) {
      const h = await new Home().up();
      h.sdk.turn = async (ctx) => {
        for (const id of ["a", "b", "c", "d", "e"]) task(ctx, id);
        await ctx.tool("Agent", agentCall);
        ctx.result();
      };
      await start(h, cap);
      await h.waitEvent("result");
      assert.equal(h.events("waiting").length, 0);
    }
  });

  it("other tools are not gated by the cap", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      task(ctx, "t1");
      await tick(20);
      await ctx.tool("Bash", { command: "ls" });
      ctx.result();
    };
    await start(h, 1);
    await h.waitEvent("result");
    assert.equal(h.events("waiting").length, 0);
  });

  it("gives up with a visible reason after the longest wait", async () => {
    const h = await new Home({ subagentWaitSec: 0.15 }).up();
    let outcome = undefined as any;
    h.sdk.turn = async (ctx) => {
      task(ctx, "t1");
      await tick(20);
      outcome = await ctx.tool("Agent", agentCall);
      ctx.result();
    };
    await start(h, 1);
    await h.waitEvent("result");
    assert.equal(outcome?.allowed, false);
    assert.match(outcome?.reason, /No place for another subagent came free/);
    assert.match((await h.waitEvent("error")).message, /waited too long/);
  });

  it("closing the session ends the wait", async () => {
    const h = await new Home().up();
    let outcome = undefined as any;
    h.sdk.turn = async (ctx) => {
      task(ctx, "t1");
      await tick(20);
      outcome = await ctx.tool("Agent", agentCall);
    };
    const { id } = await start(h, 1);
    await h.waitEvent("waiting");
    await h.ok("session.close", { id });
    await h.waitEvent("waiting", (p) => p.done === true);
    await tick(20);
    assert.equal(outcome?.allowed ?? false, false);
  });

  it("the SDK hook timeout covers the longest wait, and a bad cap is refused", async () => {
    const h = await new Home({ subagentWaitSec: 1000, hookTimeoutSec: 300 }).up();
    await start(h, 3);
    const entries = h.sdk.last.options.hooks.PreToolUse;
    assert.equal(entries[0]!.timeout, 300, "the hook that asks about every tool keeps the usual timeout");
    assert.equal(entries[0]!.matcher, undefined);
    assert.equal(entries[1]!.timeout, 1300, "the subagent gate has the longest wait on top");
    assert.equal(entries[1]!.matcher, "^(Agent|Task)$");
    const r = await h.call("session.start", { prompt: "x", mode: "default", max_subagents: -1 });
    assert.ok(r.error);
    const r2 = await h.call("session.start", { prompt: "x", mode: "default", max_subagents: "many" });
    assert.ok(r2.error);
  });

  it("a send to a running session keeps its cap", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      if (ctx.text === "hold") await ctx.hold();
      else {
        task(ctx, "t1");
        await tick(20);
        await ctx.tool("Agent", agentCall);
      }
      ctx.result();
    };
    const { id } = await start(h, 4, { prompt: "hold" });
    await h.ok("session.send", { id, text: "second", max_subagents: 1 }); // held back behind the running turn
    h.sdk.last.release();
    await h.waitFor(() => h.events("result").length === 2);
    assert.equal(h.events("waiting").length, 0);
  });

  it("a send to an idle session takes the cap it brings", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      if (ctx.text !== "plain") {
        task(ctx, "t1");
        await tick(20);
        await ctx.tool("Agent", agentCall);
      }
      ctx.result();
    };
    const { id } = await start(h, 4, { prompt: "plain" });
    await h.waitEvent("result");
    await h.waitFor(() => h.statuses().at(-1) === "idle");
    await h.ok("session.send", { id, text: "work", max_subagents: 1 });
    const w = await h.waitEvent("waiting", (p) => !p.done);
    assert.match(w.reason, /already has 1 subagent running/);
  });

  it("an interrupt ends the wait with a visible reason and leaves nothing counted", async () => {
    const h = await new Home().up();
    let outcome = undefined as any;
    h.sdk.turn = async (ctx) => {
      task(ctx, "t1");
      await tick(20);
      outcome = await ctx.tool("Agent", agentCall);
      ctx.result();
    };
    const { id } = await start(h, 1);
    await h.waitEvent("waiting", (p) => !p.done);
    await h.ok("session.interrupt", { id });
    await h.waitEvent("waiting", (p) => p.done === true);
    await h.waitEvent("result");
    assert.equal(outcome?.allowed, false);
    assert.match(outcome?.reason, /Stopped while waiting/);
    // Nothing leaked: when the one running subagent ends, the count is 0 and a new call goes straight through.
    task(h.sdk.last, "t1", true);
    await tick(30);
    assert.equal(h.events("subagents").at(-1)!.count, 0);
    h.sdk.turn = async (ctx) => {
      outcome = await ctx.tool("Agent", agentCall);
      ctx.result();
    };
    const waits = h.events("waiting").length;
    await h.ok("session.send", { id, text: "again" });
    await h.waitFor(() => h.events("result").length === 2);
    assert.equal(outcome?.allowed, true);
    assert.equal(h.events("waiting").length, waits);
  });

  it("a denied subagent call stops waiting and holds no place", async () => {
    const h = await new Home().up();
    h.askHandler = (p) => (p.tool === "Agent" ? { behavior: "deny", message: "no subagents today" } : { behavior: "allow" });
    let outcome = undefined as any;
    h.sdk.turn = async (ctx) => {
      task(ctx, "t1");
      await tick(20);
      outcome = await ctx.tool("Agent", agentCall);
      ctx.result();
    };
    await start(h, 1);
    await h.waitEvent("result");
    assert.equal(outcome?.allowed, false);
    assert.equal(outcome?.reason, "no subagents today");
    await h.waitEvent("waiting", (p) => p.done === true);
    assert.equal(h.events("subagents").at(-1)!.count, 1, "only the background task");
  });

  it("a subagent's own end releases its place without waiting for the tool result, and it is counted once", async () => {
    const h = await new Home().up();
    const never = new Promise<void>(() => {});
    let second = undefined as any;
    h.sdk.turn = async (ctx) => {
      void ctx.tool("Agent", agentCall, { holdResult: never }); // the result of this call never arrives
      await tick(30);
      const call = h.events("tool").find((e) => e.name === "Agent")!.tool_use_id;
      // The SDK reports the subagent started by that call, without saying it runs in the background.
      ctx.emit({ type: "system", subtype: "task_started", task_id: "t1", tool_use_id: call });
      await tick(30);
      second = ctx.tool("Agent", agentCall);
      await tick(60);
      assert.equal(h.events("waiting").some((e) => !e.done), true, "the second call waits");
      ctx.emit({ type: "system", subtype: "task_notification", task_id: "t1", tool_use_id: call });
      await second;
      ctx.result();
    };
    await start(h, 1);
    await h.waitEvent("result");
    assert.equal(Math.max(...h.events("subagents").map((e) => e.count)), 1, "one subagent is never counted twice");
  });

  it("a subagent's own calls are not held back by the places it and its parents hold", async () => {
    const h = await new Home().up();
    const never = new Promise<void>(() => {});
    const outcomes: any[] = [];
    h.sdk.turn = async (ctx) => {
      void ctx.tool("Agent", agentCall, { holdResult: never });
      await tick(30);
      const call = h.events("tool").find((e) => e.name === "Agent")!.tool_use_id;
      ctx.emit({ type: "system", subtype: "task_started", task_id: "t1", tool_use_id: call, is_backgrounded: true });
      await tick(30);
      // The subagent t1 starts one of its own: cap 1 would hold it for ever, waiting on its own place.
      outcomes.push(await ctx.tool("Agent", agentCall, { agentId: "t1" }));
      ctx.result();
    };
    await start(h, 1);
    await h.waitEvent("result");
    assert.equal(outcomes[0].allowed, true);
    assert.equal(h.events("waiting").length, 0);
  });

  it("a waiting call repeats its waiting event, so Home sees the session alive", async () => {
    const h = await new Home({ waitKeepaliveMs: 30 }).up();
    h.sdk.turn = async (ctx) => {
      task(ctx, "t1");
      await tick(20);
      await ctx.tool("Agent", agentCall);
      ctx.result();
    };
    await start(h, 1);
    await h.waitFor(() => h.events("waiting").filter((e) => e.still === true).length >= 2);
    assert.match(h.events("waiting")[0]!.reason, /^waiting: /);
  });

  it("every tool is still asked about, with the split of hooks", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      await ctx.tool("Bash", { command: "ls" });
      await ctx.tool("Agent", agentCall);
      ctx.result();
    };
    await start(h, 2);
    await h.waitEvent("result");
    assert.deepEqual(h.asks.map((a) => a.tool), ["Bash", "Agent"]);
  });
});
