// The Rubi agent runner: JSON-RPC over stdio in front of the Claude Agent SDK.
// Contract: docs/design/agent-runner.md. Patterns from T3 Code (MIT, see ../LICENSE-T3):
// one long-lived query per session fed by a queue, session id chosen up front, fork via
// forkSession() + resume, steering with priority "now", a permission callback kept installed.
import { createHash, randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import type { Readable, Writable } from "node:stream";
import { serveCredentials, type CredServer } from "./cred.js";
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
  settingSources?: string[]; // default ["project"] (CLAUDE.md, skills); [] ignores the project's .claude entirely
  defaultMode?: PermissionMode; // mode of a session the runner did not start itself
  helloTimeoutMs?: number; // default 10 s
  hookTimeoutSec?: number; // default 300 (SDK hook timeout)
  /** The longest a subagent call waits for a free place under the session's cap; then it is denied with a reason. Default 6 h. */
  subagentWaitSec?: number;
  /** While a subagent call waits for a place, a `waiting` event is repeated this often so Home sees the session alive. Default 5 min. */
  waitKeepaliveMs?: number;
  askTimeoutMs?: number; // default 295 s: no answer -> deny (Home itself denies after 280 s)
  maxLineBytes?: number;
  maxQueuedBytes?: number;
  /** Unix socket for git-credential-rubi; unset = no credential socket. */
  credSock?: string;
  credTimeoutMs?: number; // default 295 s (an approval can take minutes)
  /**
   * Directory of the sign-in shared by all projects (default $RUBI_AUTH_DIR, else /rubi-auth): when
   * <authDir>/token/oauth-token holds a token, every query gets it as CLAUDE_CODE_OAUTH_TOKEN. Only the token
   * is shared; the config dir stays the project's own.
   */
  authDir?: string;
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
  decided: Map<string, { d: Decision; tool: string; hashes: string[]; at: number; ttl: number }>;
  /** Restarts of a dead query without a result in between (bounded, so a crash loop ends). */
  respawns: number;
  /** Asks in flight, so an interrupt can cancel them. */
  asking: Set<AbortController>;
  /** Most subagents running at once in this session (0 = no cap); set by Home at start and at a send to an idle session. */
  maxSubagents: number;
  /** Subagent calls (Agent/Task) that passed the gate and have not ended, by tool_use_id, with the agent (if any) that made the call. With `tasks` (background ones) they are the running subagents. */
  calls: Map<string, string | undefined>;
  /** Task id -> the tool_use_id of the Agent/Task call that started it, so one subagent is counted once. */
  taskCall: Map<string, string>;
  /** Calls waiting at the gate; cancelled when the call is denied or ended some other way. */
  gating: Map<string, { cancelled: boolean }>;
  /** Raised by an interrupt: gate waits that began earlier give up. */
  stopSeq: number;
  /** Subagent calls waiting for a place; woken when the count changes. */
  gateWaiters: (() => void)[];
  /** The count last reported in a `subagents` event. */
  lastSub: number;
}

interface Decision {
  behavior: "allow" | "deny";
  message?: string;
  updated_input?: Record<string, unknown>;
}

const SHORT = 200;
const MAX_PENDING = 50;
const PUSH_WINDOW_MS = 120_000;
const GIT_PUSH_RE = /\bgit\b[^\n]*\bpush\b/;
const DECIDED_MAX = 100;
const DECIDED_TTL_MS = 30_000;
const SUBAGENT_MATCHER = "^(Agent|Task)$";
const isSubagentTool = (name: string) => name === "Agent" || name === "Task";
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

/** The shared sign-in token, or undefined when there is none (no file, empty, or not a plain token). */
export function readSharedToken(authDir: string): string | undefined {
  try {
    const t = readFileSync(`${authDir}/token/oauth-token`, "utf8").trim();
    return t.length > 0 && t.length <= 4096 && /^[\x21-\x7e]+$/.test(t) ? t : undefined;
  } catch {
    return undefined;
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

/**
 * Deny rules (checked before any hook; a hook allow cannot override them, M1 experiment):
 * `Edit(...)` covers every file-editing tool including Write (a `Write(...)` rule did not block);
 * `Read(...)` keeps the login in the config dir unreadable.
 */
export function disallowedEditTools(cwd: string, authDir?: string): string[] {
  const abs = `//${cwd.replace(/^\/+/, "")}`; // "//abs" is the SDK's absolute-path rule syntax
  const rules = [`Edit(${abs}/.rubi/**)`, `Edit(${abs}/.claude/**)`, `Edit(${abs}/.mcp.json)`, `Read(${abs}/.rubi/**)`];
  // The shared sign-in's token is not for the Read tool either (a shell can still read it: see the design).
  if (authDir) rules.push(`Read(//${authDir.replace(/^\/+/, "")}/**)`);
  return rules;
}

export class Runner {
  private readonly cfg: Required<Omit<RunnerConfig, "maxLineBytes" | "maxQueuedBytes" | "credSock" | "log" | "env" | "sdk" | "exit" | "version">>;
  private readonly sdk: Sdk;
  private readonly env: NodeJS.ProcessEnv;
  private peer!: Peer;
  private authed = false;
  private exited = false;
  private readonly sessions = new Map<string, Session>();
  private readonly childEnv: Record<string, string>;
  private credServer: CredServer | undefined;
  private pushUntil = 0;

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
      settingSources: c.settingSources ?? ["project"],
      helloTimeoutMs: c.helloTimeoutMs ?? 10_000,
      hookTimeoutSec: c.hookTimeoutSec ?? 300,
      subagentWaitSec: c.subagentWaitSec ?? 6 * 3600,
      waitKeepaliveMs: c.waitKeepaliveMs ?? 300_000,
      askTimeoutMs: c.askTimeoutMs ?? 295_000,
      credTimeoutMs: c.credTimeoutMs ?? 295_000,
      authDir: c.authDir ?? c.env.RUBI_AUTH_DIR ?? "/rubi-auth",
    };
    this.childEnv = buildChildEnv(c.env, this.cfg.configDir);
  }

  private log(m: string) {
    this.c.log?.(m.replace(/sk-ant-[A-Za-z0-9_-]+/g, "[redacted]"));
  }

  /** The environment of a `claude` child: read at each query start, so a sign-in or sign-out applies to the next one. */
  private queryEnv(): Record<string, string> {
    const token = readSharedToken(this.cfg.authDir);
    return token === undefined ? this.childEnv : { ...this.childEnv, CLAUDE_CODE_OAUTH_TOKEN: token };
  }

  private exit(code: number) {
    if (this.exited) return;
    this.exited = true;
    this.credServer?.close();
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
      ...(this.c.maxQueuedBytes !== undefined ? { maxQueuedBytes: this.c.maxQueuedBytes } : {}),
      log: (m) => this.log(m),
      onRequest: (method, params) => this.onRequest(method, params),
      onClose: () => {
        this.log("stdin closed; exiting");
        this.exit(this.authed ? 0 : EXIT_HELLO_FAILED);
      },
    });
    try {
      const r = (await this.peer.request("hello", { run_token: token, version: this.c.version }, {
        timeoutMs: this.cfg.helloTimeoutMs,
        // synchronous: a request that follows the answer in the same chunk must already find us ready
        onResult: (res) => {
          if ((res as { ok?: unknown } | null)?.ok === true) this.authed = true;
        },
      })) as { ok?: unknown } | null;
      if (r?.ok !== true) throw new Error("hello refused");
      if (this.c.credSock) {
        this.credServer = await serveCredentials(this.c.credSock, (op, input) => this.credential(op, input), (m) => this.log(m));
      }
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
        const id = uuid(p, "id");
        const limit = optInt(p, "limit");
        const messages = await this.sdk.getSessionMessages(id, { dir: this.cfg.cwd, ...(limit !== undefined ? { limit } : {}) });
        return { messages };
      }
      case "session.start":
        return this.sessionStart(p);
      case "session.send":
        return this.sessionSend(p);
      case "session.rename": {
        const id = uuid(p, "id");
        const name = str(p, "name");
        await this.sdk.renameSession(id, name, { dir: this.cfg.cwd });
        const s = this.sessions.get(id);
        if (s) s.name = name;
        return { ok: true };
      }
      case "session.fork": {
        const id = uuid(p, "id");
        const name = optStr(p, "name");
        const f = await this.sdk.forkSession(id, { dir: this.cfg.cwd, ...(name !== undefined ? { title: name } : {}) });
        const src = this.sessions.get(id);
        this.register({ id: f.sessionId, mode: src?.mode ?? this.cfg.defaultMode, ...(src ? { maxSubagents: src.maxSubagents } : {}), ...(name !== undefined ? { name } : {}) });
        return { id: f.sessionId };
      }
      case "session.interrupt": {
        const s = this.get(uuid(p, "id"));
        s.pending = []; // held-back messages are dropped: the user wants the work to stop
        s.stopSeq++; // subagent calls waiting for a place give up too
        this.updateSubagents(s);
        for (const a of [...s.asking]) a.abort(); // pending asks are given up (perm.cancel) and denied
        if (s.query) await s.query.interrupt();
        return { ok: true };
      }
      case "session.mode": {
        const s = this.get(uuid(p, "id"));
        const mode = modeOf(p, "mode");
        if (s.query) await s.query.setPermissionMode(mode);
        s.mode = mode;
        return { ok: true };
      }
      case "session.close": {
        const s = this.sessions.get(uuid(p, "id"));
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

  private register(init: { id: string; mode: PermissionMode; name?: string; maxSubagents?: number }): Session {
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
      respawns: 0,
      asking: new Set(),
      maxSubagents: init.maxSubagents ?? 0,
      calls: new Map(),
      taskCall: new Map(),
      gating: new Map(),
      stopSeq: 0,
      gateWaiters: [],
      lastSub: 0,
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
    const s = this.register({ id, mode, maxSubagents: maxSubagentsOf(p), ...(name !== undefined ? { name } : {}) });
    try {
      this.openQuery(s, "new", name);
      this.deliver(s, this.userMessage(prompt, optStr(p, "from")), false);
    } catch (e) {
      this.dropSession(s); // a failed start leaves no half-registered session
      throw e;
    }
    return { id };
  }

  private sessionSend(p: Record<string, unknown>) {
    const id = uuid(p, "id");
    const text = str(p, "text");
    const steer = p.steer === true;
    let s = this.sessions.get(id);
    const fresh = !s;
    const cap = maxSubagentsOf(p);
    if (!s) s = this.register({ id, mode: this.cfg.defaultMode, maxSubagents: cap }); // idle session from disk: resumed below
    else if (!s.turnActive && s.tasks.size === 0 && s.calls.size === 0) s.maxSubagents = cap; // a running session keeps its cap
    try {
      const delivered = this.deliver(s, this.userMessage(text, optStr(p, "from")), steer);
      return { queued: true, delivered };
    } catch (e) {
      if (fresh) this.dropSession(s);
      throw e;
    }
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
      if (s.pending.length >= MAX_PENDING) {
        this.event(s, "error", { message: `message queue full (${MAX_PENDING}); message dropped` });
        throw new RpcError(ERR.queueFull, `too many held-back messages for this session (${MAX_PENDING})`);
      }
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
      const r = await this.sdk.probe({ env: this.queryEnv(), cwd: this.cfg.cwd });
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
      settingSources: this.cfg.settingSources,
      // The project's own settings must not run code outside our PreToolUse gate: no settings hooks,
      // no apiKeyHelper (an empty string beats the project's value), no .mcp.json servers.
      settings: { disableAllHooks: true, apiKeyHelper: "" },
      strictMcpConfig: true,
      mcpServers: {},
      allowDangerouslySkipPermissions: true,
      permissionMode: s.mode,
      env: this.queryEnv(),
      disallowedTools: disallowedEditTools(this.cfg.cwd, this.cfg.authDir),
      abortController: abort,
      // The hook runs for the main session, subagents and forks, in every mode (M0 exp 2-4).
      // Every tool is asked about by the first hook, with the usual timeout. A second hook, only for the subagent
      // tools, holds the call while the session has its cap of subagents running; it has the long timeout (the
      // longest wait on top) and decides nothing else, so a matcher that did not match would lose the cap, never an ask.
      hooks: {
        PreToolUse: [
          { timeout: this.cfg.hookTimeoutSec, hooks: [(input, _id, o) => this.preToolUse(s, input, o.signal)] },
          { matcher: SUBAGENT_MATCHER, timeout: this.cfg.hookTimeoutSec + this.cfg.subagentWaitSec, hooks: [(input, _id, o) => this.subagentGateHook(s, input, o.signal)] },
        ],
      },
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
    s.calls.clear();
    s.taskCall.clear();
    this.updateSubagents(s);
    s.decided.clear();
    // Held-back messages go into a resumed query, in order, unless it keeps dying without a result.
    const next = s.pending.shift();
    if (next && !s.closed && s.respawns < 3) {
      s.respawns++;
      try {
        this.openQuery(s, "resume");
        s.turnActive = true;
        (s as Session).inbox?.push(next);
      } catch (e) {
        this.event(s, "error", { message: `could not resume the session: ${short(errMsg(e), 300)}` });
        s.pending = [];
      }
    } else if (next) {
      this.event(s, "error", { message: "the session's query keeps ending; held-back messages dropped" });
      s.pending = [];
    }
    this.updateStatus(s);
  }

  private onMessage(s: Session, m: SdkMessage) {
    s.lastActivity = Date.now();
    switch (m.type) {
      case "assistant": {
        // A top-level assistant message nobody asked for is a turn the SDK started itself (a finished
        // background task woke the model): messages sent now must wait for its result like any other.
        if (!s.turnActive && (m.parent_tool_use_id === null || m.parent_tool_use_id === undefined)) {
          s.turnActive = true;
          this.updateStatus(s);
        }
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
        s.respawns = 0;
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
      case "user": {
        // A tool result ends a foreground subagent call (a background one goes on as a task).
        const content = ((m.message ?? {}) as { content?: unknown }).content;
        let ended = false;
        for (const b of Array.isArray(content) ? (content as Record<string, unknown>[]) : []) {
          if (b.type !== "tool_result" || typeof b.tool_use_id !== "string") continue;
          if (s.calls.delete(b.tool_use_id)) ended = true;
          const g = s.gating.get(b.tool_use_id);
          if (g) g.cancelled = true; // the call ended while its gate still waited
        }
        if (ended) this.updateSubagents(s);
        break;
      }
      case "system":
        this.onSystem(s, m);
        break;
      default:
        break;
    }
  }

  private onSystem(s: Session, m: SdkMessage) {
    const before = new Map(s.tasks);
    if (typeof m.task_id === "string" && typeof m.tool_use_id === "string" && (m.subtype === "task_started" || m.subtype === "task_progress")) s.taskCall.set(m.task_id, m.tool_use_id);
    if (m.subtype === "background_tasks_changed") {
      s.tasks.clear();
      for (const t of Array.isArray(m.tasks) ? (m.tasks as Record<string, unknown>[]) : []) {
        if (t.ambient === true || typeof t.task_id !== "string") continue;
        s.tasks.set(t.task_id, taskInfo(t));
      }
    } else if (m.subtype === "task_started") {
      if (m.is_backgrounded !== false && typeof m.task_id === "string") s.tasks.set(m.task_id, taskInfo(m));
    } else if (m.subtype === "task_notification") {
      // The subagent's own end releases its place, whatever happens to the tool result of its call.
      if (typeof m.task_id === "string") {
        s.tasks.delete(m.task_id);
        s.taskCall.delete(m.task_id);
      }
      if (typeof m.tool_use_id === "string") {
        s.calls.delete(m.tool_use_id);
        const g = s.gating.get(m.tool_use_id);
        if (g) g.cancelled = true;
      }
    } else {
      return;
    }
    // One event per agent that started or finished: Home counts them to keep its slot busy.
    for (const [id, t] of s.tasks) if (!before.has(id)) this.event(s, "background", { agent_id: id, ...omitId(t) });
    for (const id of before.keys()) if (!s.tasks.has(id)) this.event(s, "background", { agent_id: id, done: true });
    this.updateSubagents(s);
    this.updateStatus(s);
  }

  /** Subagents running now: background tasks plus calls without an end, a subagent started by a call being counted once. */
  private runningSubagents(s: Session): number {
    const started = new Set<string>();
    for (const id of s.tasks.keys()) {
      const call = s.taskCall.get(id);
      if (call) started.add(call);
    }
    let n = s.tasks.size;
    for (const id of s.calls.keys()) if (!started.has(id)) n++;
    return n;
  }

  /**
   * Places held by the agent that makes a call and by the agents above it: a subagent waiting to start its own
   * subagent must not wait for the place it holds itself, or the tree would wait on itself for ever.
   */
  private heldByChain(s: Session, agentId: string | undefined): number {
    let n = 0;
    let agent = agentId;
    const seen = new Set<string>();
    while (agent && !seen.has(agent) && n < 16) {
      seen.add(agent);
      n++;
      const call = s.taskCall.get(agent);
      agent = call !== undefined ? s.calls.get(call) : undefined;
    }
    return n;
  }

  /** Reports a changed count to Home (the panel shows it) and wakes the calls waiting for a place. */
  private updateSubagents(s: Session) {
    const n = this.runningSubagents(s);
    if (n !== s.lastSub) {
      s.lastSub = n;
      this.event(s, "subagents", { count: n });
    }
    const w = s.gateWaiters;
    s.gateWaiters = [];
    for (const f of w) f();
  }

  /**
   * The gate for the subagent tools: while the session already has its cap of subagents running the call waits,
   * it is not denied. A subagent's own calls are not held back by the places it and its parents hold (the rule
   * that avoids a tree waiting on itself), so a tree can be deeper than the cap. Waiting is announced with a
   * `waiting` event, repeated every waitKeepaliveMs (Home reads any event as a sign of life, so its idle watchdog
   * leaves a waiting session alone), and ends with one carrying `done`. Returns a reason when the call must not
   * go on: interrupted or closed, denied or ended meanwhile, or waited longer than subagentWaitSec.
   */
  private async subagentGate(s: Session, input: PreToolUseInput, signal: AbortSignal): Promise<string | undefined> {
    const id = input.tool_use_id;
    const limit = Date.now() + this.cfg.subagentWaitSec * 1000;
    const stop = s.stopSeq;
    const mine = { cancelled: false };
    s.gating.set(id, mine);
    let waiting = false;
    let lastEvent = 0;
    const done = () => {
      s.gating.delete(id);
      if (waiting) this.event(s, "waiting", { reason: "", done: true, tool_use_id: id });
    };
    try {
      for (;;) {
        if (s.stopSeq !== stop || signal.aborted || s.closed) {
          done();
          return "Stopped while waiting for a free subagent place.";
        }
        if (mine.cancelled) {
          done();
          return "The call ended before a subagent place came free.";
        }
        const cap = s.maxSubagents;
        if (cap <= 0 || this.runningSubagents(s) - this.heldByChain(s, input.agent_id) < cap) break;
        const now = Date.now();
        if (now >= limit) {
          done();
          this.event(s, "error", { message: "a subagent waited too long for a free place and was not started" });
          return `No place for another subagent came free in ${this.cfg.subagentWaitSec} s (the limit is ${cap}); it was not started.`;
        }
        if (!waiting || now - lastEvent >= this.cfg.waitKeepaliveMs) {
          this.event(s, "waiting", { reason: `waiting: this session already has ${cap} subagent${cap === 1 ? "" : "s"} running, the limit`, tool_use_id: id, ...(waiting ? { still: true } : {}) });
          waiting = true;
          lastEvent = now;
        }
        await new Promise<void>((resolve) => {
          const timer = setTimeout(resolve, Math.max(1, Math.min(limit - now, lastEvent + this.cfg.waitKeepaliveMs - now)));
          timer.unref(); // a wait nobody ends must not keep the process alive
          const wake = () => {
            clearTimeout(timer);
            signal.removeEventListener("abort", wake);
            resolve();
          };
          s.gateWaiters.push(wake);
          signal.addEventListener("abort", wake, { once: true });
        });
      }
      s.calls.set(id, input.agent_id);
      this.updateSubagents(s);
      done();
      return undefined;
    } catch (e) {
      done();
      throw e;
    }
  }

  private async subagentGateHook(s: Session, input: PreToolUseInput, signal: AbortSignal): Promise<HookOutput> {
    try {
      if (input.hook_event_name !== "PreToolUse" || !isSubagentTool(input.tool_name)) return {};
      const why = await this.subagentGate(s, input, signal);
      if (why) return { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: why } };
      return {};
    } catch (e) {
      this.log(`subagent gate failed: ${errMsg(e)}; denying`);
      return { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: "Rubi could not check this call, so it was denied." } };
    }
  }

  /** A subagent call that was denied or failed: its gate stops waiting and it holds no place. */
  private dropSubagentCall(s: Session, id: string) {
    const g = s.gating.get(id);
    if (g) g.cancelled = true;
    s.calls.delete(id);
    this.updateSubagents(s);
  }

  private statusOf(s: Session): Status {
    return s.turnActive || s.tasks.size > 0 ? "busy" : "idle";
  }

  private updateStatus(s: Session) {
    const st = this.statusOf(s);
    if (st === s.lastStatus) return;
    s.lastStatus = st;
    this.event(s, "status", { status: st });
  }

  private event(s: Session, kind: string, body: Record<string, unknown>) {
    this.peer.notify("event", { session_id: s.id, kind, ...body }, kind === "assistant" || kind === "tool" ? s.id : undefined);
  }

  private dropSession(s: Session) {
    s.closed = true;
    this.sessions.delete(s.id);
    s.inbox?.end();
    s.abort?.abort();
    try {
      s.query?.close();
    } catch {
      // already gone
    }
    s.query = undefined;
  }

  private closeSession(s: Session) {
    if (s.closed) return;
    s.closed = true;
    this.sessions.delete(s.id);
    s.pending = [];
    this.updateSubagents(s); // wakes the subagent calls waiting for a place; they see the session is closed
    s.inbox?.end();
    s.abort?.abort();
    try {
      s.query?.close();
    } catch (e) {
      this.log(`close failed: ${errMsg(e)}`);
    }
    s.query = undefined;
    this.event(s, "status", { status: "closed" });
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

  // ---------------------------------------------------------------- git credentials

  /**
   * Relays one request of git-credential-rubi to Home. The purpose is "push" only while the runner
   * knows a push is under way: Home approved a perm.ask for a `git ... push` command a moment ago.
   * Anything else is "read" (Home gives reads and pushes different credentials).
   */
  private async credential(op: string, input: string): Promise<string> {
    if (!this.authed || this.peer.isClosed) throw new Error("not connected to Rubi Home");
    const purpose = Date.now() < this.pushUntil ? "push" : "read";
    const r = (await this.peer.request("git.credential", { op, input, purpose }, { timeoutMs: this.cfg.credTimeoutMs })) as { output?: unknown } | null;
    return typeof r?.output === "string" ? r.output : "";
  }

  private notePush(tool: string, input: unknown, d: Decision) {
    if (d.behavior !== "allow" || tool !== "Bash") return;
    const cmd = (d.updated_input ?? (input as Record<string, unknown> | null))?.command;
    if (typeof cmd === "string" && GIT_PUSH_RE.test(cmd)) this.pushUntil = Date.now() + PUSH_WINDOW_MS;
  }

  // ---------------------------------------------------------------- permissions

  private async preToolUse(s: Session, input: PreToolUseInput, signal: AbortSignal): Promise<HookOutput> {
    try {
      if (input.hook_event_name !== "PreToolUse") return {};
      // AskUserQuestion is decided in canUseTool (it carries the answers back); asking twice would
      // show the same question twice.
      if (input.tool_name === "AskUserQuestion") return {};
      const subagent = isSubagentTool(input.tool_name);
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
      this.remember(s, input.tool_use_id, input.tool_name, input.tool_input, d);
      this.notePush(input.tool_name, input.tool_input, d);
      if (d.behavior === "deny") {
        if (subagent) this.dropSubagentCall(s, input.tool_use_id);
        return { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: d.message ?? "Denied by Rubi" } };
      }
      // An allow is never returned from the hook: the SDK's own rules and canUseTool still apply
      // after it. A changed input goes back without a decision (M1 experiment: the SDK applies it).
      if (d.updated_input) return { hookSpecificOutput: { hookEventName: "PreToolUse", updatedInput: d.updated_input } };
      return {};
    } catch (e) {
      if (isSubagentTool(input.tool_name)) this.dropSubagentCall(s, input.tool_use_id);
      this.log(`PreToolUse hook failed: ${errMsg(e)}; denying`);
      return { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: "Rubi could not check this call, so it was denied." } };
    }
  }

  private async canUseTool(s: Session, tool: string, input: Record<string, unknown>, o: { signal: AbortSignal; toolUseID?: string; agentID?: string }): Promise<CanUseToolResult> {
    try {
      if (tool === "AskUserQuestion") this.event(s, "ask", { tool_use_id: o.toolUseID, input });
      else {
        const prev = this.recall(s, o.toolUseID, tool, input);
        if (prev) {
          // The hook already got Home's decision for this very call: no second question.
          if (prev.behavior === "deny") return { behavior: "deny", message: prev.message ?? "Denied by Rubi" };
          return { behavior: "allow", updatedInput: prev.updated_input ?? input };
        }
      }
      const d = await this.askHome(
        s,
        { tool, input, ...(o.toolUseID ? { tool_use_id: o.toolUseID } : {}), ...(o.agentID ? { agent_id: o.agentID } : {}), cwd: this.cfg.cwd, via: "can_use_tool" },
        o.signal,
      );
      this.notePush(tool, input, d);
      if (d.behavior === "deny") return { behavior: "deny", message: d.message ?? "Denied by Rubi" };
      return { behavior: "allow", updatedInput: d.updated_input ?? input };
    } catch (e) {
      this.log(`canUseTool failed: ${errMsg(e)}; denying`);
      return { behavior: "deny", message: "Rubi could not check this call, so it was denied." };
    }
  }

  /** Decisions the hook got, keyed by tool_use_id and matched by tool name and input hash. */
  private remember(s: Session, id: string | undefined, tool: string, input: unknown, d: Decision) {
    if (!id) return;
    const now = Date.now();
    for (const [k, v] of s.decided) if (now - v.at > v.ttl) s.decided.delete(k);
    // A subagent call may wait at the gate long after it was approved: its decision is kept for that long.
    const ttl = isSubagentTool(tool) ? DECIDED_TTL_MS + this.cfg.subagentWaitSec * 1000 : DECIDED_TTL_MS;
    s.decided.set(id, { d, tool, hashes: [hashOf(input), ...(d.updated_input ? [hashOf(d.updated_input)] : [])], at: now, ttl });
    while (s.decided.size > DECIDED_MAX) s.decided.delete(s.decided.keys().next().value as string);
  }

  private recall(s: Session, id: string | undefined, tool: string, input: unknown): Decision | undefined {
    if (!id) return undefined;
    const e = s.decided.get(id);
    if (!e) return undefined;
    s.decided.delete(id);
    if (e.tool !== tool || Date.now() - e.at > e.ttl || !e.hashes.includes(hashOf(input))) return undefined;
    return e.d;
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

function hashOf(v: unknown): string {
  return createHash("sha256").update(JSON.stringify(v) ?? "").digest("hex");
}

function omitId(t: { task_id: string; subagent_type?: string; description?: string }) {
  const { task_id: _id, ...rest } = t;
  return rest;
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
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
function uuid(p: Record<string, unknown>, k: string): string {
  const v = p[k];
  return typeof v === "string" && UUID_RE.test(v) ? v : bad(k);
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
/** `max_subagents` from Home: 0 or absent = no cap. */
function maxSubagentsOf(p: Record<string, unknown>): number {
  const v = p.max_subagents;
  if (v === undefined || v === null) return 0;
  return typeof v === "number" && Number.isInteger(v) && v >= 0 && v <= 1000 ? v : bad("max_subagents");
}
function modeOf(p: Record<string, unknown>, k: string): PermissionMode {
  const v = p[k];
  return typeof v === "string" && (PERMISSION_MODES as readonly string[]).includes(v) ? (v as PermissionMode) : bad(k);
}
