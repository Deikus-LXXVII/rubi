// Opt-in (RUBI_RUNNER_INTEGRATION=1): the real runner process, the real SDK and the real bundled
// `claude` binary, with a scripted local API instead of the model (ANTHROPIC_BASE_URL). No login,
// no tokens, no network beyond 127.0.0.1. Takes a minute or two.
import assert from "node:assert/strict";
import { spawn, execFileSync, type ChildProcessWithoutNullStreams } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { describe, it, before, after } from "node:test";
import { fileURLToPath } from "node:url";
import { TOKEN } from "./harness.js";

const enabled = process.env.RUBI_RUNNER_INTEGRATION === "1";
const here = path.dirname(fileURLToPath(import.meta.url));
type Msg = Record<string, any>;

class Child {
  readonly lines: Msg[] = [];
  readonly stderr: string[] = [];
  private buf = "";
  private nextId = 1;
  private waiters: { pred: (m: Msg) => boolean; resolve: (m: Msg) => void }[] = [];
  askHandler: (p: Msg) => Msg = () => ({ behavior: "allow" });
  readonly asks: Msg[] = [];
  exitCode: number | null = null;

  constructor(private readonly p: ChildProcessWithoutNullStreams) {
    p.stdout.on("data", (c: Buffer) => {
      this.buf += c.toString("utf8");
      let i;
      while ((i = this.buf.indexOf("\n")) >= 0) {
        const m = JSON.parse(this.buf.slice(0, i)) as Msg;
        this.buf = this.buf.slice(i + 1);
        this.lines.push(m);
        this.onLine(m);
        this.waiters = this.waiters.filter((w) => (w.pred(m) ? (w.resolve(m), false) : true));
      }
    });
    p.stderr.on("data", (c: Buffer) => this.stderr.push(c.toString("utf8")));
    p.on("exit", (c) => (this.exitCode = c));
  }
  private onLine(m: Msg) {
    if (m.method === "hello") this.send({ jsonrpc: "2.0", id: m.id, result: m.params.run_token === TOKEN ? { ok: true } : null });
    else if (m.method === "perm.ask") {
      this.asks.push(m.params);
      this.send({ jsonrpc: "2.0", id: m.id, result: this.askHandler(m.params) });
    }
  }
  send(m: Msg) {
    this.p.stdin.write(JSON.stringify(m) + "\n");
  }
  async call(method: string, params: Msg): Promise<Msg> {
    const id = 1000 + this.nextId++;
    const r = this.waitFor((m) => m.id === id);
    this.send({ jsonrpc: "2.0", id, method, params });
    const m = await r;
    if (m.error) throw new Error(`${method}: ${JSON.stringify(m.error)}; runner log: ${this.stderr.join("").slice(-600)}`);
    return m.result;
  }
  waitFor(pred: (m: Msg) => boolean, timeoutMs = 90_000): Promise<Msg> {
    const f = this.lines.find(pred);
    if (f) return Promise.resolve(f);
    return new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error(`timeout; events: ${JSON.stringify(this.events().map((e) => e.kind))}; stderr: ${this.stderr.join("").slice(-1500)}`)), timeoutMs);
      this.waiters.push({ pred, resolve: (m) => (clearTimeout(t), resolve(m)) });
    });
  }
  events(kind?: string, sid?: string): Msg[] {
    return this.lines.filter((m) => m.method === "event" && (!kind || m.params.kind === kind) && (!sid || m.params.session_id === sid)).map((m) => m.params);
  }
  result(sid: string, n: number): Promise<Msg> {
    return this.waitFor(() => this.events("result", sid).length >= n).then(() => this.events("result", sid)[n - 1]!);
  }
  kill() {
    this.p.stdin.end();
    this.p.kill("SIGTERM");
  }
}

describe("real SDK + real claude against the scripted API", { skip: !enabled }, () => {
  let mock: { url: string; requests: any[]; close: () => void };
  let work: string, home: string, child: Child;
  const marker = () => path.join(work, "marker.txt");

  before(async () => {
    const { startMock } = (await import(path.join(here, "../../test/support/mock.mjs"))) as any;
    mock = await startMock({
      allow: [{ tools: [{ name: "Bash", input: { command: "echo allowed > marker.txt", description: "marker" } }] }, { text: "ALLOWED-DONE" }],
      deny: [{ tools: [{ name: "Bash", input: { command: "echo denied > marker2.txt", description: "marker" } }] }, { text: "DENIED-DONE" }],
      sub: [{ tools: [{ name: "$AGENT", input: { subagent_type: "general-purpose", description: "sub work", prompt: "MOCKSCRIPT:subwork do it" } }] }, { text: "SUB-LAUNCHED" }],
      subwork: [{ tools: [{ name: "Bash", input: { command: "echo sub > marker3.txt", description: "sub" } }] }, { text: "SUB-DONE" }],
      ask: [{ tools: [{ name: "AskUserQuestion", input: { questions: [{ question: "Which one?", header: "Pick", multiSelect: false, options: [{ label: "A", description: "a" }, { label: "B", description: "b" }] }] } }] }, { text: "ASKED-DONE" }],
      two: [{ text: "TWO-DONE" }],
      one: [{ text: "ONE-DONE" }],
    });
    work = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "runner-work-"))); // sessions are keyed by real path
    home = fs.mkdtempSync(path.join(os.tmpdir(), "runner-home-")); // keeps the host's own settings out of the way
    execFileSync("git", ["init", "-q", "-b", "main", work]);
    const env = { PATH: process.env.PATH ?? "", HOME: home, RUBI_RUN_TOKEN: TOKEN, RUBI_WORK_DIR: work, ANTHROPIC_BASE_URL: mock.url, ANTHROPIC_API_KEY: "mock-placeholder-not-a-real-key" };
    child = new Child(spawn(process.execPath, [path.join(here, "../src/main.js")], { env, stdio: "pipe" }));
    await child.waitFor((m) => m.method === "hello");
  });

  after(async () => {
    child?.kill();
    for (let i = 0; i < 50 && child && child.exitCode === null; i++) await new Promise((r) => setTimeout(r, 100));
    mock?.close();
    for (const d of [work, home]) if (d) fs.rmSync(d, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 });
  });

  it("allow: the tool call is asked through the hook and then runs", async () => {
    child.askHandler = () => ({ behavior: "allow" });
    const { id } = await child.call("session.start", { prompt: "MOCKSCRIPT:allow run it", mode: "bypassPermissions" });
    const r = await child.result(id, 1);
    assert.equal(r.is_error, false);
    const a = child.asks.find((x) => x.session_id === id && x.tool === "Bash")!;
    assert.ok(a, "perm.ask for Bash");
    assert.equal(a.via, "hook");
    assert.equal(a.cwd, work);
    assert.equal(fs.existsSync(marker()), true);
    assert.ok(child.events("tool", id).some((e) => e.name === "Bash"));
  });

  it("deny: the call does not run, in bypass mode", async () => {
    child.askHandler = () => ({ behavior: "deny", message: "denied by the test" });
    const { id } = await child.call("session.start", { prompt: "MOCKSCRIPT:deny run it", mode: "bypassPermissions" });
    await child.result(id, 1);
    assert.equal(fs.existsSync(path.join(work, "marker2.txt")), false);
    const seen = JSON.stringify(mock.requests.filter((r) => r.script === "deny").flatMap((r) => r.saw_tool_results));
    assert.match(seen, /denied by the test/);
  });

  it("subagent: the hook sees agent_id and a deny applies inside it", async () => {
    child.askHandler = (p) => (p.agent_id ? { behavior: "deny", message: "no subagent bash" } : { behavior: "allow" });
    const { id } = await child.call("session.start", { prompt: "MOCKSCRIPT:sub delegate", mode: "bypassPermissions" });
    await child.waitFor(() => child.asks.some((x) => x.session_id === id && x.agent_id), 90_000);
    const a = child.asks.find((x) => x.session_id === id && x.agent_id)!;
    assert.ok(a.agent_type);
    assert.equal(fs.existsSync(path.join(work, "marker3.txt")), false);
    await child.waitFor(() => child.events("status", id).at(-1)?.state === "idle" && child.events("result", id).length >= 1);
  });

  it("AskUserQuestion reaches Home as an ask event and a perm.ask", async () => {
    child.askHandler = (p) => (p.tool === "AskUserQuestion" ? { behavior: "allow", updated_input: { ...p.input, answers: { "Which one?": "A" } } } : { behavior: "allow" });
    const { id } = await child.call("session.start", { prompt: "MOCKSCRIPT:ask question", mode: "bypassPermissions" });
    await child.result(id, 1);
    assert.ok(child.events("ask", id).length >= 1, "ask event");
    const a = child.asks.find((x) => x.session_id === id && x.tool === "AskUserQuestion");
    assert.ok(a, "perm.ask for AskUserQuestion");
    assert.equal(a.via, "can_use_tool"); // the SDK does consult canUseTool for it even in bypass mode
  });

  it("a second message in the same live session is answered; close then send resumes the session", async () => {
    child.askHandler = () => ({ behavior: "allow" });
    const { id } = await child.call("session.start", { prompt: "MOCKSCRIPT:one first", mode: "bypassPermissions" });
    await child.result(id, 1);
    await child.call("session.send", { id, text: "MOCKSCRIPT:two second" });
    await child.result(id, 2);
    await child.call("session.close", { id });
    await child.call("session.send", { id, text: "MOCKSCRIPT:one third" });
    await child.waitFor(() => child.events("result", id).length >= 3);
    const list = (await child.call("session.list", {})) as { sessions: Msg[] };
    assert.ok(list.sessions.some((s) => s.id === id));
  });

  it("fork makes a new session that answers", async () => {
    const { id } = await child.call("session.start", { prompt: "MOCKSCRIPT:one first", mode: "bypassPermissions" });
    await child.result(id, 1);
    const f = await child.call("session.fork", { id, name: "branch" });
    assert.notEqual(f.id, id);
    await child.call("session.send", { id: f.id, text: "MOCKSCRIPT:two in the fork" });
    await child.result(f.id, 1);
  });

  it("usage.probe answers without a model call", async () => {
    const before = mock.requests.length;
    const r = await child.call("usage.probe", {});
    assert.equal(typeof r.logged_in, "boolean");
    assert.equal(mock.requests.length, before);
  });
});
