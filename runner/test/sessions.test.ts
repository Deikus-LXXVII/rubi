import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { Home, tick } from "./harness.js";

const start = (h: Home, extra: Record<string, unknown> = {}) => h.ok("session.start", { prompt: "go", mode: "bypassPermissions", ...extra });

describe("perm.ask from the PreToolUse hook", () => {
  it("round trip: allow lets the call through and carries everything Home needs", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      const r = await ctx.tool("Bash", { command: "ls" });
      ctx.say(r.allowed ? "ran" : "blocked");
      ctx.result();
    };
    const { id } = await start(h);
    await h.waitEvent("result");
    const a = h.asks[0]!;
    assert.match(a.ask_id, /^[0-9a-f-]{36}$/);
    assert.equal(a.session_id, id);
    assert.equal(a.tool, "Bash");
    assert.deepEqual(a.input, { command: "ls" });
    assert.equal(a.cwd, "/work");
    assert.equal(a.mode, "bypassPermissions");
    assert.equal(a.agent_id, undefined);
    assert.equal(a.via, "hook");
    assert.equal(h.events("assistant").at(-1)!.text, "ran");
  });

  it("round trip: deny blocks the call and the model sees the reason", async () => {
    const h = await new Home().up();
    h.askHandler = () => ({ behavior: "deny", message: "no pushing" });
    let outcome: unknown;
    h.sdk.turn = async (ctx) => {
      outcome = await ctx.tool("Bash", { command: "git push" });
      ctx.result();
    };
    await start(h);
    await h.waitEvent("result");
    assert.deepEqual(outcome, { allowed: false, reason: "no pushing" });
  });

  it("updated_input from Home is returned through the hook", async () => {
    const h = await new Home().up();
    h.askHandler = () => ({ behavior: "allow", updated_input: { command: "ls -la" } });
    let hookOut: unknown;
    h.sdk.turn = async (ctx) => {
      const hook = ctx.q.options.hooks.PreToolUse[0]!.hooks[0]!;
      hookOut = await hook({ hook_event_name: "PreToolUse", session_id: "s", cwd: "/work", tool_name: "Bash", tool_input: { command: "ls" }, tool_use_id: "t1" }, "t1", { signal: new AbortController().signal });
      ctx.result();
    };
    await start(h);
    await h.waitEvent("result");
    assert.deepEqual(hookOut, { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "allow", updatedInput: { command: "ls -la" } } });
  });

  it("every tool call of a subagent is asked with agent_id and agent_type, and deny applies there", async () => {
    const h = await new Home().up();
    h.askHandler = (p) => (p.agent_id ? { behavior: "deny", message: "subagents may not" } : { behavior: "allow" });
    const out: unknown[] = [];
    h.sdk.turn = async (ctx) => {
      out.push(await ctx.tool("Bash", { command: "echo main" }));
      out.push(await ctx.tool("Bash", { command: "git push" }, { agentId: "agent-1", agentType: "pusher" }));
      ctx.result();
    };
    await start(h);
    await h.waitEvent("result");
    assert.equal(h.asks.length, 2);
    assert.equal(h.asks[1]!.agent_id, "agent-1");
    assert.equal(h.asks[1]!.agent_type, "pusher");
    assert.deepEqual(out, [{ allowed: true }, { allowed: false, reason: "subagents may not" }]);
  });

  it("is asked in a forked session too (fork then send)", async () => {
    const h = await new Home().up();
    h.askHandler = () => ({ behavior: "deny", message: "fork denied" });
    let outcome: unknown;
    h.sdk.turn = async (ctx) => {
      outcome = await ctx.tool("Write", { file_path: "/tmp/x", content: "x" });
      ctx.result();
    };
    const { id } = await start(h);
    await h.waitEvent("result");
    const f = await h.ok("session.fork", { id, name: "branch" });
    await h.ok("session.send", { id: f.id, text: "again" });
    await h.waitFor((l) => l.msg.method === "event" && l.msg.params.kind === "result" && l.msg.params.session_id === f.id);
    assert.equal(h.asks.at(-1)!.session_id, f.id);
    assert.deepEqual(outcome, { allowed: false, reason: "fork denied" });
  });

  it("denies when Home does not answer in time (runner side ask timeout)", async () => {
    const h = await new Home({ askTimeoutMs: 60 }).up();
    h.askHandler = () => new Promise(() => {}); // never answers
    let outcome: any;
    h.sdk.turn = async (ctx) => {
      outcome = await ctx.tool("Bash", { command: "echo hi" });
      ctx.result();
    };
    await start(h);
    await h.waitEvent("result");
    assert.equal(outcome.allowed, false);
    assert.match(outcome.reason, /did not answer in time/);
  });

  it("the SDK's hook timeout (simulated) blocks the call and aborts the pending ask", async () => {
    const h = await new Home().up();
    h.askHandler = () => new Promise(() => {});
    let outcome: any;
    h.sdk.turn = async (ctx) => {
      outcome = await ctx.tool("Bash", { command: "echo hi" }, { hookTimeoutMs: 50 });
      ctx.result();
    };
    await start(h);
    await h.waitEvent("result");
    assert.equal(outcome.timedOut, true);
    assert.equal(outcome.allowed, false);
    assert.equal(h.sdk.last.options.hooks.PreToolUse[0]!.timeout, 300);
  });

  it("denies a call that was asked when Home goes away", async () => {
    const h = await new Home().up();
    h.askHandler = () => new Promise(() => {});
    let outcome: any;
    h.sdk.turn = async (ctx) => {
      outcome = await ctx.tool("Bash", { command: "echo hi" });
    };
    await start(h);
    await h.waitFor((l) => l.msg.method === "perm.ask");
    h.closeStdin();
    await tick(30);
    assert.equal(outcome?.allowed, false);
  });

  it("an answer that is not an explicit allow is a deny", async () => {
    const h = await new Home().up();
    h.askHandler = () => ({ behavior: "maybe" });
    let outcome: any;
    h.sdk.turn = async (ctx) => {
      outcome = await ctx.tool("Bash", { command: "x" });
      ctx.result();
    };
    await start(h);
    await h.waitEvent("result");
    assert.equal(outcome.allowed, false);
  });
});

describe("canUseTool and AskUserQuestion", () => {
  it("routes canUseTool through perm.ask with tool set and returns updated input", async () => {
    const h = await new Home().up();
    h.askHandler = () => ({ behavior: "allow", updated_input: { answers: { q1: "yes" } } });
    let r: unknown;
    h.sdk.turn = async (ctx) => {
      r = await ctx.canUseTool("AskUserQuestion", { questions: [{ question: "q1" }] });
      ctx.result();
    };
    await start(h);
    await h.waitEvent("result");
    assert.equal(h.asks[0]!.tool, "AskUserQuestion");
    assert.equal(h.asks[0]!.via, "can_use_tool");
    assert.deepEqual(r, { behavior: "allow", updatedInput: { answers: { q1: "yes" } } });
    const ask = h.events("ask")[0]!;
    assert.deepEqual(ask.input, { questions: [{ question: "q1" }] });
    // the ask event comes before the perm.ask so the plugin can show the question
    const iAsk = h.lines.findIndex((l) => l.msg.params?.kind === "ask");
    const iPerm = h.lines.findIndex((l) => l.msg.method === "perm.ask");
    assert.ok(iAsk < iPerm);
  });

  it("the hook leaves AskUserQuestion to canUseTool (no double question)", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      await ctx.tool("AskUserQuestion", { questions: [] });
      ctx.result();
    };
    await start(h);
    await h.waitEvent("result");
    assert.equal(h.asks.length, 0);
  });

  it("canUseTool deny carries the message", async () => {
    const h = await new Home().up();
    h.askHandler = () => ({ behavior: "deny", message: "stricter mode says no" });
    let r: unknown;
    h.sdk.turn = async (ctx) => {
      r = await ctx.canUseTool("Edit", { file_path: "/work/a" });
      ctx.result();
    };
    await start(h, { mode: "default" });
    await h.waitEvent("result");
    assert.deepEqual(r, { behavior: "deny", message: "stricter mode says no" });
  });
});

describe("send, queue, steer", () => {
  it("a message sent while a turn runs is held back until the result, then delivered", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      if (ctx.text === "first") await ctx.hold();
      ctx.say("done " + ctx.text);
      ctx.result();
    };
    const { id } = await start(h, { prompt: "first" });
    await h.waitEvent("status", (e) => e.state === "busy");
    const r = await h.ok("session.send", { id, text: "second" });
    assert.deepEqual(r, { queued: true, delivered: false });
    await tick(30);
    assert.deepEqual(h.sdk.last.received.map((x) => x.text), ["first"]); // not handed to the SDK mid-turn
    h.sdk.last.release();
    await h.waitFor((l) => l.msg.params?.kind === "assistant" && l.msg.params.text === "done second");
    assert.deepEqual(h.sdk.last.received.map((x) => [x.text, x.priority]), [["first", undefined], ["second", undefined]]);
    await h.waitFor((l) => l.msg.params?.kind === "status" && l.msg.params.state === "idle" && h.events("result").length === 2);
  });

  it("several queued messages are released one per turn, in order", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      if (ctx.text === "a") await ctx.hold();
      ctx.result();
    };
    const { id } = await start(h, { prompt: "a" });
    await h.ok("session.send", { id, text: "b" });
    await h.ok("session.send", { id, text: "c" });
    h.sdk.last.release();
    await h.waitFor(() => h.events("result").length === 3);
    assert.deepEqual(h.sdk.last.received.map((x) => x.text), ["a", "b", "c"]);
  });

  it("steer goes in at once with priority now", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      if (ctx.text === "long") await ctx.hold();
      ctx.result();
    };
    const { id } = await start(h, { prompt: "long" });
    const r = await h.ok("session.send", { id, text: "change course", steer: true });
    assert.deepEqual(r, { queued: true, delivered: true });
    await tick(20);
    assert.deepEqual(h.sdk.last.received.map((x) => [x.text, x.priority]), [["long", undefined], ["change course", "now"]]);
    h.sdk.last.release();
  });

  it("send to an idle live session reuses the same query (no resume)", async () => {
    const h = await new Home().up();
    const { id } = await start(h);
    await h.waitEvent("result");
    await h.waitFor(() => h.statuses().at(-1) === "idle");
    await h.ok("session.send", { id, text: "more" });
    await h.waitFor(() => h.events("result").length === 2);
    assert.equal(h.sdk.queries.length, 1);
    assert.equal(h.sdk.last.received.length, 2);
  });

  it("send to a session the runner does not hold resumes it by id", async () => {
    const h = await new Home().up();
    const id = "11111111-2222-3333-4444-555555555555";
    const r = await h.ok("session.send", { id, text: "welcome back" });
    assert.deepEqual(r, { queued: true, delivered: true });
    const q = h.sdk.last;
    assert.equal(q.options.resume, id);
    assert.equal(q.options.sessionId, undefined);
    assert.equal(q.options.permissionMode, "default");
    await h.waitFor((l) => l.msg.params?.kind === "result" && l.msg.params.session_id === id);
  });

  it("resumes after a close: the next send opens a new query with resume", async () => {
    const h = await new Home().up();
    const { id } = await start(h);
    await h.waitEvent("result");
    await h.ok("session.close", { id });
    await h.ok("session.send", { id, text: "again" });
    assert.equal(h.sdk.queries.length, 2);
    assert.equal(h.sdk.last.options.resume, id);
  });

  it("the sender is named in the text when from is given", async () => {
    const h = await new Home().up();
    await h.ok("session.start", { prompt: "hello", from: "alice", mode: "default" });
    assert.equal(h.sdk.last.received[0]!.text, "[message from alice]\nhello");
  });
});

describe("fork, rename, interrupt, mode, close, list, messages", () => {
  it("fork uses forkSession, registers an idle session and opens no query until sent to", async () => {
    const h = await new Home().up();
    const { id } = await start(h, { mode: "plan" });
    await h.waitEvent("result");
    const before = h.sdk.queries.length;
    const f = await h.ok("session.fork", { id, name: "branch" });
    assert.notEqual(f.id, id);
    assert.deepEqual(h.sdk.forks, [{ id, title: "branch", newId: f.id }]);
    assert.equal(h.sdk.queries.length, before);
    await h.ok("session.send", { id: f.id, text: "in the fork" });
    assert.equal(h.sdk.last.options.resume, f.id);
    assert.equal(h.sdk.last.options.permissionMode, "plan"); // the fork inherits the source's mode
  });

  it("rename calls renameSession", async () => {
    const h = await new Home().up();
    const { id } = await start(h);
    assert.deepEqual(await h.ok("session.rename", { id, name: "new name" }), { ok: true });
    assert.deepEqual(h.sdk.renames, [{ id, title: "new name" }]);
  });

  it("interrupt reaches the query; unknown session is an error", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      await ctx.hold();
    };
    const { id } = await start(h);
    assert.deepEqual(await h.ok("session.interrupt", { id }), { ok: true });
    assert.equal(h.sdk.last.interrupted, 1);
    assert.equal((await h.call("session.interrupt", { id: "nope" })).error.code, -32004);
    h.sdk.last.release();
  });

  it("mode change goes to the live query and is used by the next perm.ask and the next resume", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      await ctx.tool("Bash", { command: "echo" });
      ctx.result();
    };
    const { id } = await start(h, { mode: "bypassPermissions" });
    await h.waitEvent("result");
    assert.deepEqual(await h.ok("session.mode", { id, mode: "plan" }), { ok: true });
    assert.deepEqual(h.sdk.last.modes, ["plan"]);
    await h.ok("session.send", { id, text: "next" });
    await h.waitFor(() => h.events("result").length === 2);
    assert.equal(h.asks.at(-1)!.mode, "plan");
    assert.equal((await h.call("session.mode", { id, mode: "bogus" })).error.code, -32602);
    await h.ok("session.close", { id });
    await h.ok("session.send", { id, text: "after close" });
    assert.equal(h.sdk.last.options.permissionMode, "default"); // closed = forgotten; Home sets the mode again
  });

  it("close ends the query, drops queued messages and emits status closed; close is idempotent", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      await ctx.hold();
    };
    const { id } = await start(h);
    await h.ok("session.send", { id, text: "queued" });
    assert.deepEqual(await h.ok("session.close", { id }), { ok: true });
    assert.equal(h.sdk.last.closed, true);
    assert.equal(h.statuses().at(-1), "closed");
    assert.deepEqual(await h.ok("session.close", { id }), { ok: true });
    assert.equal(h.sdk.last.received.length, 1);
  });

  it("list merges the disk list with the registry (name, mode, busy)", async () => {
    const h = await new Home().up();
    h.sdk.sessions.push({ sessionId: "disk-1", summary: "from disk", lastModified: 5000, createdAt: 1000 });
    h.sdk.turn = async (ctx) => {
      await ctx.hold();
    };
    const { id } = await start(h, { name: "live one", mode: "acceptEdits" });
    const { sessions } = (await h.ok("session.list")) as { sessions: any[] };
    const disk = sessions.find((s) => s.id === "disk-1");
    assert.deepEqual(disk, { id: "disk-1", name: "from disk", created: 1000, last_activity: 5000, mode: "default", busy: false });
    const live = sessions.find((s) => s.id === id);
    assert.equal(live.name, "live one");
    assert.equal(live.mode, "acceptEdits");
    assert.equal(live.busy, true);
    h.sdk.last.release();
  });

  it("messages passes the limit through", async () => {
    const h = await new Home().up();
    const r = (await h.ok("session.messages", { id: "abc", limit: 1 })) as { messages: unknown[] };
    assert.equal(r.messages.length, 1);
    assert.equal((await h.call("session.messages", { id: "abc", limit: -1 })).error.code, -32602);
  });
});

describe("events", () => {
  it("emits assistant, tool, result (with usage), and busy/idle status in order", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      ctx.say("thinking out loud");
      await ctx.tool("Bash", { command: "ls -la" });
      ctx.result();
    };
    const { id } = await start(h);
    await h.waitFor(() => h.statuses().at(-1) === "idle" && h.events("result").length === 1);
    const kinds = h.events().map((e) => e.kind);
    assert.deepEqual(kinds, ["status", "assistant", "tool", "result", "status"]);
    assert.equal(h.events("assistant")[0]!.text, "thinking out loud");
    assert.deepEqual(h.events("tool")[0], { session_id: id, kind: "tool", name: "Bash", summary: "ls -la", tool_use_id: "toolu_1" });
    const res = h.events("result")[0]!;
    assert.deepEqual(res.usage, { input_tokens: 100, output_tokens: 20 });
    assert.equal(res.total_cost_usd, 0.0001);
    assert.equal(res.num_turns, 1);
    assert.equal(res.is_error, false);
    assert.ok(res.model_usage["fake-model"]);
    assert.deepEqual(h.statuses(), ["busy", "idle"]);
  });

  it("subagent messages carry parent_tool_use_id", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      ctx.say("from the subagent", "toolu_parent");
      ctx.result();
    };
    await start(h);
    await h.waitEvent("result");
    assert.equal(h.events("assistant")[0]!.parent_tool_use_id, "toolu_parent");
  });

  it("long tool input is shortened in the event", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      await ctx.tool("Bash", { command: "echo " + "z".repeat(5000) });
      ctx.result();
    };
    await start(h);
    await h.waitEvent("result");
    assert.ok(h.events("tool")[0]!.summary.length < 260);
  });

  it("rate_limit from rate_limit_event and from api_error_params", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      ctx.emit({ type: "rate_limit_event", rate_limit_info: { status: "allowed_warning", rateLimitType: "five_hour", utilization: 0.9 } });
      ctx.emit({ type: "assistant", message: { content: [] }, api_error_params: { rate_limit_info: { status: "rejected" } } });
      ctx.result({ is_error: true });
    };
    await start(h);
    await h.waitEvent("result");
    const rl = h.events("rate_limit");
    assert.deepEqual(rl.map((e) => e.info.status), ["allowed_warning", "rejected"]);
    assert.equal(h.events("result")[0]!.is_error, true);
  });

  it("background subagent keeps the session busy until its own end; background events say so", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      ctx.emit({ type: "system", subtype: "background_tasks_changed", tasks: [{ task_id: "t1", subagent_type: "researcher", description: "dig", task_type: "local_agent" }, { task_id: "amb", ambient: true }] });
      ctx.result();
    };
    const { id } = await start(h);
    await h.waitEvent("result");
    await tick(20);
    assert.deepEqual(h.statuses(), ["busy"]); // the turn is over, the subagent is not
    assert.deepEqual(h.events("background")[0]!.tasks, [{ task_id: "t1", subagent_type: "researcher", description: "dig" }]);
    const l = (await h.ok("session.list")) as { sessions: any[] };
    assert.equal(l.sessions.find((s) => s.id === id).busy, true);
    h.sdk.last.emit({ type: "system", subtype: "task_notification", task_id: "t1", status: "completed" });
    await h.waitFor(() => h.statuses().at(-1) === "idle");
    assert.deepEqual(h.events("background").at(-1)!.tasks, []);
  });

  it("task_started / task_notification work without the level message", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      ctx.emit({ type: "system", subtype: "task_started", task_id: "t9", is_backgrounded: true, subagent_type: "x", description: "d" });
      ctx.emit({ type: "system", subtype: "task_started", task_id: "fg", is_backgrounded: false });
      ctx.result();
    };
    await start(h);
    await h.waitEvent("result");
    assert.deepEqual(h.events("background")[0]!.tasks.map((t: any) => t.task_id), ["t9"]);
  });

  it("a query that ends without a result makes the session idle again, and it can be resumed", async () => {
    const h = await new Home().up();
    h.sdk.turn = (ctx) => {
      ctx.q.out.end(); // the claude process went away mid-turn
    };
    const { id } = await start(h);
    await h.waitFor(() => h.statuses().join() === "busy,idle");
    h.sdk.turn = (ctx) => ctx.result();
    await h.ok("session.send", { id, text: "retry" });
    assert.equal(h.sdk.last.options.resume, id);
    await h.waitEvent("result");
  });

  it("an error thrown by the query stream itself is reported", async () => {
    const h = await new Home().up();
    h.sdk.query = () => ({
      async interrupt() {},
      async setPermissionMode() {},
      close() {},
      [Symbol.asyncIterator]() {
        return { next: () => Promise.reject(new Error("claude exited with code 1")) };
      },
    });
    await start(h);
    const e = await h.waitEvent("error");
    assert.match(e.message, /claude exited/);
    await h.waitFor(() => h.statuses().at(-1) === "idle");
  });
});

describe("usage.probe", () => {
  it("not logged in", async () => {
    const h = await new Home().up();
    assert.deepEqual(await h.ok("usage.probe"), { logged_in: false });
  });
  it("logged in with a plan and limits, no email", async () => {
    const h = await new Home().up();
    h.sdk.probeResult = { account: { subscriptionType: "max", tokenSource: "claude.ai", apiProvider: "firstParty" }, limits: { five_hour: { utilization: 12 } } };
    const r = (await h.ok("usage.probe")) as Record<string, any>;
    assert.equal(r.logged_in, true);
    assert.equal(r.plan, "max");
    assert.deepEqual(r.limits, { five_hour: { utilization: 12 } });
    assert.equal(JSON.stringify(r).includes("@"), false);
    assert.equal(h.sdk.queries.length, 0); // no query, so no tokens
  });
  it("a failing probe is reported as not logged in, not as a crash", async () => {
    const h = await new Home().up();
    h.sdk.probeError = new Error("could not start claude");
    const r = (await h.ok("usage.probe")) as Record<string, any>;
    assert.equal(r.logged_in, false);
    assert.match(r.error, /could not start/);
  });
});
