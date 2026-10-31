// A scripted "Rubi Home" for tests: the other end of the runner's stdio pipes.
import { PassThrough } from "node:stream";
import { EXIT_HELLO_FAILED, Runner, type RunnerConfig } from "../src/runner.js";
import { FakeSdk } from "./fake-sdk.js";

export type Msg = Record<string, any>;
export interface Line {
  from: "home" | "runner";
  msg: Msg;
  /** Not a JSON-RPC message: a control step ("close_stdin", "exit") or a raw (malformed) line. */
  action?: string;
  raw?: string;
}

export const TOKEN = "t".repeat(64);
export const tick = (ms = 10) => new Promise((r) => setTimeout(r, ms));

export class Home {
  readonly toRunner = new PassThrough();
  readonly fromRunner = new PassThrough();
  readonly lines: Line[] = []; // everything on the wire, in order
  readonly exits: number[] = [];
  readonly logs: string[] = [];
  readonly sdk: FakeSdk;
  runner: Runner;
  /** Answers every perm.ask unless a test replaces it. */
  askHandler: (p: Msg) => Promise<Msg> | Msg = () => ({ behavior: "allow" });
  readonly asks: Msg[] = [];
  private nextId = 1;
  private waiters: { pred: (l: Line) => boolean; resolve: (l: Line) => void }[] = [];
  private pendingCalls = new Map<number, (m: Msg) => void>();
  private buf = "";
  helloHandler: (p: Msg) => Msg | null = (p) => (p.run_token === TOKEN ? { ok: true } : null);

  constructor(cfg: Partial<RunnerConfig> & { env?: NodeJS.ProcessEnv; sdk?: FakeSdk; maxLineBytes?: number } = {}) {
    this.sdk = cfg.sdk ?? new FakeSdk();
    this.fromRunner.on("data", (c: Buffer) => this.onData(c.toString("utf8")));
    this.runner = new Runner(this.toRunner, this.fromRunner, {
      sdk: this.sdk,
      version: "test",
      env: { PATH: "/usr/bin", RUBI_RUN_TOKEN: TOKEN, HTTPS_PROXY: "http://proxy.invalid:3128", ...cfg.env },
      exit: (c) => this.exits.push(c),
      log: (m) => this.logs.push(m),
      cwd: "/work",
      ...cfg,
    });
  }

  private onData(s: string) {
    this.buf += s;
    let i;
    while ((i = this.buf.indexOf("\n")) >= 0) {
      const line = this.buf.slice(0, i);
      this.buf = this.buf.slice(i + 1);
      const msg = JSON.parse(line) as Msg;
      this.record({ from: "runner", msg });
      void this.handle(msg);
    }
  }

  private record(l: Line) {
    this.lines.push(l);
    if (l.action || l.raw) return;
    this.waiters = this.waiters.filter((w) => {
      if (!w.pred(l)) return true;
      w.resolve(l);
      return false;
    });
  }

  private async handle(msg: Msg) {
    if (msg.method === "hello") {
      const r = this.helloHandler(msg.params);
      if (r) this.reply(msg.id, r);
      else this.closeStdin();
    } else if (msg.method === "perm.ask") {
      this.asks.push(msg.params);
      const r = await this.askHandler(msg.params);
      this.reply(msg.id, r);
    } else if (msg.id !== undefined && ("result" in msg || "error" in msg)) {
      this.pendingCalls.get(msg.id)?.(msg);
      this.pendingCalls.delete(msg.id);
    }
  }

  reply(id: number, result: Msg) {
    this.sendRaw({ jsonrpc: "2.0", id, result });
  }
  sendRaw(msg: unknown) {
    this.record({ from: "home", msg: msg as Msg });
    this.toRunner.write(JSON.stringify(msg) + "\n");
  }
  /** Bytes straight onto the wire (malformed lines, huge lines); not recorded. */
  writeBytes(s: string | Buffer) {
    if (typeof s === "string" && s.endsWith("\n") && s.length < 200) this.record({ from: "home", msg: {}, raw: s.slice(0, -1) });
    this.toRunner.write(s);
  }
  closeStdin() {
    this.record({ from: "home", msg: {}, action: "close_stdin" });
    this.toRunner.end();
  }

  /** Starts the runner and completes the handshake. */
  async up(): Promise<this> {
    const started = this.runner.start();
    await this.waitFor((l) => l.from === "runner" && l.msg.method === "hello");
    await started;
    return this;
  }

  /** A request to the runner; resolves with the full response message. */
  call(method: string, params: Msg = {}): Promise<Msg> {
    const id = this.nextId++;
    return new Promise((resolve) => {
      this.pendingCalls.set(id, resolve);
      this.sendRaw({ jsonrpc: "2.0", id, method, params });
    });
  }
  /** A request that must succeed: returns result. */
  async ok(method: string, params: Msg = {}): Promise<Msg> {
    const r = await this.call(method, params);
    if (r.error) throw new Error(`${method}: ${JSON.stringify(r.error)}`);
    return r.result;
  }

  waitFor(pred: (l: Line) => boolean, timeoutMs = 3000): Promise<Line> {
    const found = this.lines.find(pred);
    if (found) return Promise.resolve(found);
    return new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error("timeout waiting for a wire message; saw: " + JSON.stringify(this.lines.map((l) => l.msg.method ?? l.msg.id)))), timeoutMs);
      this.waiters.push({ pred, resolve: (l) => (clearTimeout(t), resolve(l)) });
    });
  }
  /** Waits for a runner event notification (kind, optionally more fields). */
  waitEvent(kind: string, match: (p: Msg) => boolean = () => true, from = 0): Promise<Msg> {
    return this.waitFor((l) => l.from === "runner" && l.msg.method === "event" && l.msg.params.kind === kind && match(l.msg.params) && this.lines.indexOf(l) >= from).then((l) => l.msg.params);
  }
  events(kind?: string): Msg[] {
    return this.lines.filter((l) => l.from === "runner" && l.msg.method === "event" && (!kind || l.msg.params.kind === kind)).map((l) => l.msg.params);
  }
  statuses(): string[] {
    return this.events("status").map((e) => e.state);
  }
  get mark(): number {
    return this.lines.length;
  }
}

export { EXIT_HELLO_FAILED };
