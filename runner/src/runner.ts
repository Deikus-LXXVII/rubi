// The Rubi agent runner: JSON-RPC over stdio in front of the Claude Agent SDK.
// Contract: docs/design/agent-runner.md. Patterns from T3 Code (MIT, see ../LICENSE-T3):
// one long-lived query per session fed by a queue, session id chosen up front, fork via
// forkSession() + resume, steering with priority "now", a permission callback kept installed.
import { randomUUID } from "node:crypto";
import type { Readable, Writable } from "node:stream";
import { Inbox } from "./inbox.js";
import { ERR, errMsg, Peer, RpcError } from "./protocol.js";
import {
  PERMISSION_MODES,
  type CanUseToolResult,
  type HookOutput,
  type PermissionMode,
  type PreToolUseInput,
  type QueryOptions,
  type Sdk,
  type SdkMessage,
  type SdkQuery,
  type SdkUserMessage,
} from "./sdk.js";

export interface RunnerConfig {
  sdk: Sdk;
  version: string;
  /** The runner's own environment (process.env). RUBI_RUN_TOKEN is read from it and removed. */
  env: NodeJS.ProcessEnv;
  exit: (code: number) => void;
  log?: (msg: string) => void;
  cwd?: string; // default /work
  configDir?: string; // default <cwd>/.rubi/claude
  defaultMode?: PermissionMode; // mode of a session the runner did not start itself
  helloTimeoutMs?: number; // default 10 s
  hookTimeoutSec?: number; // default 300 (SDK hook timeout)
  askTimeoutMs?: number; // default 295 s: no answer -> deny (Home itself denies after 280 s)
  maxLineBytes?: number;
}

export const EXIT_NO_TOKEN = 2;
export const EXIT_HELLO_FAILED = 3;

type Status = "idle" | "busy";

interface Session {
  id: string;
  name?: string;
  mode: PermissionMode;
  created: number;
  lastActivity: number;
  query?: SdkQuery;
  inbox?: Inbox<SdkUserMessage>;
  abort?: AbortController;
  turnActive: boolean;
  pending: SdkUserMessage[];
  tasks: Map<string, { task_id: string; subagent_type?: string; description?: string }>;
  lastStatus: Status;
  closed: boolean;
  /** Decisions the hook got, by tool_use_id, so canUseTool for the same call does not ask again. */
  decided: Map<string, Decision>;
  /** Asks in flight, so an interrupt can cancel them. */
  asking: Set<AbortController>;
}

interface Decision {
  behavior: "allow" | "deny";
  message?: string;
  updated_input?: Record<string, unknown>;
}

const SHORT = 200;
const short = (v: unknown, n = SHORT): string => {
  const s = typeof v === "string" ? v : (JSON.stringify(v) ?? "");
  return s.length > n ? s.slice(0, n) + "..." : s;
};

export function toolSummary(name: string, input: unknown): string {
  const i = (typeof input === "object" && input !== null ? input : {}) as Record<string, unknown>;
  switch (name) {
    case "Bash":
      return short(i.command);
    case "Read":
    case "Write":
    case "Edit":
    case "MultiEdit":
    case "NotebookEdit":
      return short(i.file_path ?? i.notebook_path);
    case "WebFetch":
      return short(i.url);
    case "Agent":
    case "Task":
      return short(`${i.subagent_type ?? ""}: ${i.description ?? ""}`);
    default:
      return short(input);
  }
}

/** Environment of every `claude` child: explicit, never the runner's whole env (M0 surprise 1). */
export function buildChildEnv(env: NodeJS.ProcessEnv, configDir: string): Record<string, string> {
  const out: Record<string, string> = {};
  const keep = ["PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "LANG", "LC_ALL", "TERM", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "NO_PROXY", "no_proxy"];
  for (const k of keep) {
    const v = env[k];
    if (v !== undefined) out[k] = v;
  }
  Object.assign(out, {
    CLAUDE_CONFIG_DIR: configDir,
    DISABLE_TELEMETRY: "1",
    DISABLE_ERROR_REPORTING: "1",
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
    GIT_TERMINAL_PROMPT: "0",
  });
  const proxy = env.HTTPS_PROXY ?? env.https_proxy;
  if (proxy) {
    out.HTTPS_PROXY = proxy;
    out.https_proxy = proxy;
  }
  return out;
}

export function disallowedEditTools(cwd: string): string[] {
  const p = `//${cwd.replace(/^\/+/, "")}/.rubi/**`; // "//abs" is the SDK's absolute-path rule syntax
  return ["Edit", "Write", "MultiEdit", "NotebookEdit"].map((t) => `${t}(${p})`);
}

export class Runner {
  private readonly cfg: Required<Omit<RunnerConfig, "maxLineBytes" | "log" | "env" | "sdk" | "exit" | "version">>;
  private readonly sdk: Sdk;
  private readonly env: NodeJS.ProcessEnv;
  private peer!: Peer;
  private authed = false;
  private exited = false;
  private readonly sessions = new Map<string, Session>();
  private readonly childEnv: Record<string, string>;

  constructor(
    private readonly input: Readable,
    private readonly output: Writable,
    private readonly c: RunnerConfig,
  ) {
    this.sdk = c.sdk;
    this.env = c.env;
    const cwd = c.cwd ?? "/work";
    this.cfg = {
      cwd,
      configDir: c.configDir ?? `${cwd}/.rubi/claude`,
      defaultMode: c.defaultMode ?? "default",
      helloTimeoutMs: c.helloTimeoutMs ?? 10_000,
      hookTimeoutSec: c.hookTimeoutSec ?? 300,
      askTimeoutMs: c.askTimeoutMs ?? 295_000,
    };
    this.childEnv = buildChildEnv(c.env, this.cfg.configDir);
  }

  private log(m: string) {
    this.c.log?.(m);
  }

  private exit(code: number) {
    if (this.exited) return;
    this.exited = true;
    this.shutdownSessions();
    this.c.exit(code);
  }

  /** Runs the handshake. Resolves when the runner is ready (or has asked to exit). */
  async start(): Promise<void> {
    const token = this.env.RUBI_RUN_TOKEN;
    delete this.env.RUBI_RUN_TOKEN; // never reaches a child or a log
    if (!token) {
      this.log("RUBI_RUN_TOKEN is not set; exiting");
      this.exit(EXIT_NO_TOKEN);
      return;
    }
    this.peer = new Peer(this.input, this.output, {
      ...(this.c.maxLineBytes !== undefined ? { maxLineBytes: this.c.maxLineBytes } : {}),
      log: (m) => this.log(m),
      onRequest: (method, params) => this.onRequest(method, params),
      onClose: () => {
        this.log("stdin closed; exiting");
        this.exit(this.authed ? 0 : EXIT_HELLO_FAILED);
      },
    });
    try {
      const r = (await this.peer.request("hello", { run_token: token, version: this.c.version }, { timeoutMs: this.cfg.helloTimeoutMs })) as { ok?: unknown } | null;
      if (r?.ok !== true) throw new Error("hello refused");
      this.authed = true;
    } catch (e) {
      this.log(`hello failed: ${errMsg(e)}`);
      this.exit(EXIT_HELLO_FAILED);
    }
  }

  // ---------------------------------------------------------------- requests from Home

  private async onRequest(method: string, p: Record<string, unknown>): Promise<unknown> {
    if (!this.authed) throw new RpcError(ERR.notReady, "not authenticated yet");
    switch (method) {
      case "session.list":
        return this.sessionList();
      case "session.messages": {
        const id = str(p, "id");
        const limit = optInt(p, "limit");
        const messages = await this.sdk.getSessionMessages(id, { dir: this.cfg.cwd, ...(limit !== undefined ? { limit } : {}) });
        return { messages };
      }
      case "session.start":
        return this.sessionStart(p);
      case "session.send":
        return this.sessionSend(p);
      case "session.rename": {
        const id = str(p, "id");
        const name = str(p, "name");
        await this.sdk.renameSession(id, name, { dir: this.cfg.cwd });
        const s = this.sessions.get(id);
        if (s) s.name = name;
        return { ok: true };
      }
      case "session.fork": {
        const id = str(p, "id");
        const name = optStr(p, "name");
        const f = await this.sdk.forkSession(id, { dir: this.cfg.cwd, ...(name !== undefined ? { title: name } : {}) });
        const src = this.sessions.get(id);
        this.register({ id: f.sessionId, mode: src?.mode ?? this.cfg.defaultMode, ...(name !== undefined ? { name } : {}) });
        return { id: f.sessionId };
      }
      case "session.interrupt": {
        const s = this.get(str(p, "id"));
        for (const a of [...s.asking]) a.abort(); // pending asks are given up (perm.cancel) and denied
        if (s.query) await s.query.interrupt();
        return { ok: true };
      }
      case "session.mode": {
        const s = this.get(str(p, "id"));
        const mode = modeOf(p, "mode");
        if (s.query) await s.query.setPermissionMode(mode);
        s.mode = mode;
        return { ok: true };
      }
      case "session.close": {
        const s = this.sessions.get(str(p, "id"));
        if (s) this.closeSession(s);
        return { ok: true };
      }
      case "usage.probe":
        return this.usageProbe();
      default:
        throw new RpcError(ERR.methodNotFound, `unknown method ${method}`);
    }
  }

  private get(id: string): Session {
    const s = this.sessions.get(id);
    if (!s) throw new RpcError(ERR.sessionNotFound, "no such session in this runner");
    return s;
  }

  private register(init: { id: string; mode: PermissionMode; name?: string }): Session {
    const now = Date.now();
    const s: Session = {
      id: init.id,
      ...(init.name !== undefined ? { name: init.name } : {}),
      mode: init.mode,
      created: now,
      lastActivity: now,
      turnActive: false,
      pending: [],
      tasks: new Map(),
      lastStatus: "idle",
      closed: false,
      decided: new Map(),
      asking: new Set(),
    };
    this.sessions.set(s.id, s);
    return s;
  }

  private async sessionList() {
    const infos = await this.sdk.listSessions({ dir: this.cfg.cwd });
    const seen = new Set<string>();
    const sessions = infos.map((i) => {
      seen.add(i.sessionId);
      const s = this.sessions.get(i.sessionId);
      return {
        id: i.sessionId,
        name: s?.name ?? i.customTitle ?? i.summary,
        created: i.createdAt ?? i.lastModified,
        last_activity: Math.max(i.lastModified, s?.lastActivity ?? 0),
        mode: s?.mode ?? this.cfg.defaultMode,
        busy: s ? this.statusOf(s) === "busy" : false,
      };
    });
    for (const s of this.sessions.values()) {
      if (seen.has(s.id)) continue; // started or forked but not on disk yet
      sessions.push({ id: s.id, name: s.name ?? "", created: s.created, last_activity: s.lastActivity, mode: s.mode, busy: this.statusOf(s) === "busy" });
    }
    return { sessions };
  }

  private sessionStart(p: Record<string, unknown>) {
    const prompt = str(p, "prompt");
    const name = optStr(p, "name");
    const mode = modeOf(p, "mode");
    const id = randomUUID();
    const s = this.register({ id, mode, ...(name !== undefined ? { name } : {}) });
    this.openQuery(s, "new", name);
    this.deliver(s, this.userMessage(prompt, optStr(p, "from")), false);
    return { id };
  }

  private sessionSend(p: Record<string, unknown>) {
    const id = str(p, "id");
    const text = str(p, "text");
    const steer = p.steer === true;
    let s = this.sessions.get(id);
    if (!s) s = this.register({ id, mode: this.cfg.defaultMode }); // idle session from disk: resumed below
    const delivered = this.deliver(s, this.userMessage(text, optStr(p, "from")), steer);
    return { queued: true, delivered };
  }

  private userMessage(text: string, from?: string): SdkUserMessage {
    const content = from ? `[message from ${from}]\n${text}` : text;
    return { type: "user", message: { role: "user", content }, parent_tool_use_id: null };
  }

  /**
   * Hands a message to the query. A message sent while a turn runs is held back here until the
   * turn's result (the SDK never answers a mid-turn message of default priority, M0 exp 6);
   * steer=true goes in at once with priority "now". Returns whether it reached the SDK.
   */
  private deliver(s: Session, msg: SdkUserMessage, steer: boolean): boolean {
    if (!s.query) this.openQuery(s, "resume");
    s.lastActivity = Date.now();
    if (s.turnActive && !steer) {
      s.pending.push(msg);
      return false;
    }
    if (s.turnActive && steer) msg.priority = "now";
    s.turnActive = true;
    s.inbox?.push(msg);
    this.updateStatus(s);
    return true;
  }

  private async usageProbe() {
    try {
      const r = await this.sdk.probe({ env: this.childEnv, cwd: this.cfg.cwd });
      const a = r.account ?? {};
      const none = (v?: string) => v === undefined || v === "none";
      const logged_in = !!a.subscriptionType || !none(a.tokenSource) || !none(a.apiKeySource);
      return {
        logged_in,
        ...(logged_in ? { account: { token_source: a.tokenSource, api_key_source: a.apiKeySource, api_provider: a.apiProvider } } : {}),
        ...(a.subscriptionType ? { plan: a.subscriptionType } : {}),
        ...(r.limits !== undefined ? { limits: r.limits } : {}),
      };
    } catch (e) {
      return { logged_in: false, error: short(errMsg(e), 300) };
    }
  }

  // ---------------------------------------------------------------- the query of a session

  private openQuery(s: Session, how: "new" | "resume", title?: string) {
    const inbox = new Inbox<SdkUserMessage>();
    const abort = new AbortController();
    const options: QueryOptions = {
      cwd: this.cfg.cwd,
      ...(how === "new" ? { sessionId: s.id, ...(title !== undefined ? { title } : {}) } : { resume: s.id }),
      settingSources: ["project"],
      allowDangerouslySkipPermissions: true,
      permissionMode: s.mode,
      env: this.childEnv,
      disallowedTools: disallowedEditTools(this.cfg.cwd),
      abortController: abort,
      // The hook runs for the main session, subagents and forks, in every mode (M0 exp 2-4).
      hooks: { PreToolUse: [{ timeout: this.cfg.hookTimeoutSec, hooks: [(input, _id, o) => this.preToolUse(s, input, o.signal)] }] },
      canUseTool: (tool, input, o) => this.canUseTool(s, tool, input, o),
      stderr: (d) => this.log(`claude: ${short(d.trim(), 400)}`),
    };
    const q = this.sdk.query({ prompt: inbox, options });
    s.query = q;
    s.inbox = inbox;
    s.abort = abort;
    void this.pump(s, q);
  }

  private async pump(s: Session, q: SdkQuery) {
    try {
      for await (const m of q) {
        if (s.query !== q) break;
        try {
          this.onMessage(s, m);
        } catch (e) {
          this.log(`message handler failed: ${errMsg(e)}`);
        }
      }
    } catch (e) {
      if (!s.closed && s.query === q) this.event(s, "error", { message: short(errMsg(e), 500) });
    }
    if (s.query !== q) return; // replaced or closed
    s.query = undefined;
    s.inbox = undefined;
    s.abort = undefined;
    s.turnActive = false;
    s.tasks.clear();
    this.updateStatus(s);
  }

  private onMessage(s: Session, m: SdkMessage) {
    s.lastActivity = Date.now();
    switch (m.type) {
      case "assistant": {
        const msg = (m.message ?? {}) as { content?: unknown };
        const parent = typeof m.parent_tool_use_id === "string" ? { parent_tool_use_id: m.parent_tool_use_id } : {};
        for (const b of Array.isArray(msg.content) ? (msg.content as Record<string, unknown>[]) : []) {
          if (b.type === "text" && typeof b.text === "string") this.event(s, "assistant", { text: b.text, ...parent });
          else if (b.type === "tool_use" && typeof b.name === "string") this.event(s, "tool", { name: b.name, summary: toolSummary(b.name, b.input), tool_use_id: b.id, ...parent });
        }
        const info = (m.api_error_params as { rate_limit_info?: unknown } | undefined)?.rate_limit_info ?? (msg as { api_error_params?: { rate_limit_info?: unknown } }).api_error_params?.rate_limit_info;
        if (info !== undefined) this.event(s, "rate_limit", { info });
        break;
      }
      case "result":
        this.event(s, "result", {
          subtype: m.subtype,
          usage: m.usage,
          model_usage: m.modelUsage,
          total_cost_usd: m.total_cost_usd,
          num_turns: m.num_turns,
          is_error: m.is_error === true,
        });
        s.turnActive = false;
        {
          const next = s.pending.shift();
          if (next) {
            s.turnActive = true;
            s.inbox?.push(next);
          }
        }
        this.updateStatus(s);
        break;
      case "rate_limit_event":
        this.event(s, "rate_limit", { info: m.rate_limit_info });
        break;
      case "system":
        this.onSystem(s, m);
        break;
      default:
        break;
    }
  }

  private onSystem(s: Session, m: SdkMessage) {
    const before = [...s.tasks.keys()].join();
    if (m.subtype === "background_tasks_changed") {
      s.tasks.clear();
      for (const t of Array.isArray(m.tasks) ? (m.tasks as Record<string, unknown>[]) : []) {
        if (t.ambient === true || typeof t.task_id !== "string") continue;
        s.tasks.set(t.task_id, taskInfo(t));
      }
    } else if (m.subtype === "task_started") {
      if (m.is_backgrounded !== false && typeof m.task_id === "string") s.tasks.set(m.task_id, taskInfo(m));
    } else if (m.subtype === "task_notification") {
      if (typeof m.task_id === "string") s.tasks.delete(m.task_id);
    } else {
      return;
    }
    if ([...s.tasks.keys()].join() !== before) {
      this.event(s, "background", { tasks: [...s.tasks.values()] });
      this.updateStatus(s);
    }
  }

  private statusOf(s: Session): Status {
    return s.turnActive || s.tasks.size > 0 ? "busy" : "idle";
  }

  private updateStatus(s: Session) {
    const st = this.statusOf(s);
    if (st === s.lastStatus) return;
    s.lastStatus = st;
    this.event(s, "status", { state: st });
  }

  private event(s: Session, kind: string, body: Record<string, unknown>) {
    this.peer.notify("event", { session_id: s.id, kind, ...body });
  }

  private closeSession(s: Session) {
    if (s.closed) return;
    s.closed = true;
    this.sessions.delete(s.id);
    s.pending = [];
    s.inbox?.end();
    s.abort?.abort();
    try {
      s.query?.close();
    } catch (e) {
      this.log(`close failed: ${errMsg(e)}`);
    }
    s.query = undefined;
    this.event(s, "status", { state: "closed" });
  }

  private shutdownSessions() {
    for (const s of [...this.sessions.values()]) {
      s.closed = true;
      s.inbox?.end();
      s.abort?.abort();
      try {
        s.query?.close();
      } catch {
        // already gone
      }
    }
    this.sessions.clear();
  }

  // ---------------------------------------------------------------- permissions

  private async preToolUse(s: Session, input: PreToolUseInput, signal: AbortSignal): Promise<HookOutput> {
    if (input.hook_event_name !== "PreToolUse") return {};
    // AskUserQuestion is decided in canUseTool (it carries the answers back); asking twice would
    // show the same question twice.
    if (input.tool_name === "AskUserQuestion") return {};
    const d = await this.askHome(
      s,
      {
        tool: input.tool_name,
        input: input.tool_input,
        tool_use_id: input.tool_use_id,
        ...(input.agent_id ? { agent_id: input.agent_id } : {}),
        ...(input.agent_type ? { agent_type: input.agent_type } : {}),
        cwd: input.cwd,
        via: "hook",
      },
      signal,
    );
    if (input.tool_use_id) {
      s.decided.set(input.tool_use_id, d);
      if (s.decided.size > 500) s.decided.delete(s.decided.keys().next().value as string);
    }
    if (d.behavior === "deny") {
      return { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: d.message ?? "Denied by Rubi" } };
    }
    if (d.updated_input) {
      return { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "allow", updatedInput: d.updated_input } };
    }
    return {};
  }

  private async canUseTool(s: Session, tool: string, input: Record<string, unknown>, o: { signal: AbortSignal; toolUseID?: string; agentID?: string }): Promise<CanUseToolResult> {
    if (tool === "AskUserQuestion") this.event(s, "ask", { tool_use_id: o.toolUseID, input });
    else if (o.toolUseID && s.decided.has(o.toolUseID)) {
      // The hook already got Home's decision for this very call: no second question.
      const prev = s.decided.get(o.toolUseID)!;
      s.decided.delete(o.toolUseID);
      if (prev.behavior === "deny") return { behavior: "deny", message: prev.message ?? "Denied by Rubi" };
      return { behavior: "allow", updatedInput: prev.updated_input ?? input };
    }
    const d = await this.askHome(
      s,
      { tool, input, ...(o.toolUseID ? { tool_use_id: o.toolUseID } : {}), ...(o.agentID ? { agent_id: o.agentID } : {}), cwd: this.cfg.cwd, via: "can_use_tool" },
      o.signal,
    );
    if (d.behavior === "deny") return { behavior: "deny", message: d.message ?? "Denied by Rubi" };
    return { behavior: "allow", updatedInput: d.updated_input ?? input };
  }

  /** Sends perm.ask and waits. Anything but an explicit allow is a deny. */
  private async askHome(s: Session, p: Record<string, unknown>, signal: AbortSignal): Promise<Decision> {
    const own = new AbortController();
    s.asking.add(own);
    const both = AbortSignal.any([signal, own.signal, ...(s.abort ? [s.abort.signal] : [])]);
    const ask_id = randomUUID();
    try {
      const r = (await this.peer.request("perm.ask", { ask_id, session_id: s.id, ...p, mode: s.mode }, { timeoutMs: this.cfg.askTimeoutMs, signal: both })) as Record<string, unknown> | null;
      if (r?.behavior === "allow") {
        const ui = r.updated_input;
        return { behavior: "allow", ...(ui && typeof ui === "object" && !Array.isArray(ui) ? { updated_input: ui as Record<string, unknown> } : {}) };
      }
      return { behavior: "deny", message: typeof r?.message === "string" && r.message ? r.message : "Denied by Rubi" };
    } catch (e) {
      const timedOut = e instanceof RpcError && e.code === ERR.timeout;
      const why = timedOut ? "Rubi did not answer in time" : both.aborted ? "The call was cancelled" : "Rubi could not be reached";
      this.log(`perm.ask failed (${errMsg(e)}); denying`);
      // We gave up (timeout, SDK hook timeout, interrupt, close): tell Home to drop the pending item.
      // Not when Home is gone: there is nobody to tell.
      if (!this.peer.isClosed && (timedOut || both.aborted)) this.peer.notify("perm.cancel", { ask_id, session_id: s.id });
      return { behavior: "deny", message: `${why}, so the call was denied.` };
    } finally {
      s.asking.delete(own);
    }
  }
}

function taskInfo(t: Record<string, unknown>) {
  return {
    task_id: String(t.task_id),
    ...(typeof t.subagent_type === "string" ? { subagent_type: t.subagent_type } : {}),
    ...(typeof t.description === "string" ? { description: short(t.description, 120) } : {}),
  };
}

function bad(k: string): never {
  throw new RpcError(ERR.invalidParams, `invalid or missing parameter: ${k}`);
}
function str(p: Record<string, unknown>, k: string): string {
  const v = p[k];
  return typeof v === "string" && v !== "" ? v : bad(k);
}
function optStr(p: Record<string, unknown>, k: string): string | undefined {
  const v = p[k];
  if (v === undefined || v === null) return undefined;
  return typeof v === "string" ? v : bad(k);
}
function optInt(p: Record<string, unknown>, k: string): number | undefined {
  const v = p[k];
  if (v === undefined || v === null) return undefined;
  return typeof v === "number" && Number.isInteger(v) && v > 0 ? v : bad(k);
}
function modeOf(p: Record<string, unknown>, k: string): PermissionMode {
  const v = p[k];
  return typeof v === "string" && (PERMISSION_MODES as readonly string[]).includes(v) ? (v as PermissionMode) : bad(k);
}
