// A fake Agent SDK: scripted query streams that call the runner's hook and canUseTool the way the
// real CLI does. No process, no network.
import { randomUUID } from "node:crypto";
import { Inbox } from "../src/inbox.js";
import type {
  CanUseToolResult,
  PermissionMode,
  ProbeResult,
  QueryOptions,
  Sdk,
  SdkMessage,
  SdkQuery,
  SdkUserMessage,
  SessionInfo,
} from "../src/sdk.js";

export interface ToolOpts {
  agentId?: string;
  agentType?: string;
  /** Simulates the SDK's own hook timeout: if the hook has not answered by then the call is blocked. */
  hookTimeoutMs?: number;
}
export interface ToolOutcome {
  id?: string;
  allowed: boolean;
  reason?: string;
  timedOut?: boolean;
}

export interface TurnCtx {
  text: string;
  priority: string | undefined;
  q: FakeQuery;
  emit(m: SdkMessage): void;
  say(text: string, parent?: string): void;
  tool(name: string, input: Record<string, unknown>, o?: ToolOpts): Promise<ToolOutcome>;
  canUseTool(name: string, input: Record<string, unknown>, toolUseID?: string): Promise<CanUseToolResult>;
  result(extra?: Record<string, unknown>): void;
  /** Resolves when the test calls q.release(); lets a turn stay "busy". */
  hold(): Promise<void>;
}
export type TurnFn = (ctx: TurnCtx) => Promise<void> | void;

export class FakeQuery implements SdkQuery {
  readonly out = new Inbox<SdkMessage>();
  readonly received: { text: string; priority: string | undefined }[] = [];
  readonly modes: PermissionMode[] = [];
  interrupted = 0;
  closed = false;
  private releaseFns: (() => void)[] = [];
  private turns = 0;
  private active = 0;
  private seq = 0;
  readonly sessionId: string;

  constructor(
    readonly options: QueryOptions,
    prompt: AsyncIterable<SdkUserMessage>,
    private readonly turn: TurnFn,
  ) {
    this.sessionId = options.sessionId ?? options.resume ?? randomUUID();
    void this.consume(prompt);
  }

  private async consume(prompt: AsyncIterable<SdkUserMessage>) {
    this.emit({ type: "system", subtype: "init", session_id: this.sessionId });
    const running: Promise<void>[] = [];
    // Turns run one at a time (as in the CLI); a "now" message is handed to the running turn.
    let chain: Promise<void> = Promise.resolve();
    for await (const u of prompt) {
      const text = u.message.content;
      this.received.push({ text, priority: u.priority });
      this.turns++;
      this.active++;
      chain = chain.then(() => this.runTurn(text, u.priority)).finally(() => this.active--);
      running.push(chain);
    }
    await Promise.allSettled(running);
    this.out.end();
  }

  private async runTurn(text: string, priority: string | undefined) {
    const ctx: TurnCtx = {
      text,
      priority,
      q: this,
      emit: (m) => this.emit(m),
      say: (t, parent) => this.emit({ type: "assistant", parent_tool_use_id: parent ?? null, message: { content: [{ type: "text", text: t }] } }),
      tool: (n, i, o) => this.runTool(n, i, o),
      canUseTool: (n, i, id) => this.options.canUseTool(n, i, { signal: this.options.abortController.signal, toolUseID: id ?? `toolu_${++this.seq}` }),
      result: (extra) =>
        this.emit({
          type: "result",
          subtype: "success",
          is_error: false,
          num_turns: 1,
          total_cost_usd: 0.0001,
          usage: { input_tokens: 100, output_tokens: 20 },
          modelUsage: { "fake-model": { inputTokens: 100, outputTokens: 20, costUSD: 0.0001 } },
          session_id: this.sessionId,
          ...extra,
        }),
      hold: () => new Promise<void>((r) => this.releaseFns.push(r)),
    };
    try {
      await this.turn(ctx);
    } catch (e) {
      this.out.push({ type: "result", subtype: "error_during_execution", is_error: true, errors: [String(e)] });
    }
  }

  /** Lets turns waiting in hold() continue. */
  release() {
    const fns = this.releaseFns;
    this.releaseFns = [];
    fns.forEach((f) => f());
  }

  private async runTool(name: string, input: Record<string, unknown>, o: ToolOpts = {}): Promise<ToolOutcome> {
    const id = `toolu_${++this.seq}`;
    this.emit({
      type: "assistant",
      parent_tool_use_id: o.agentId ? "toolu_parent" : null,
      message: { content: [{ type: "tool_use", id, name, input }] },
    });
    const hook = this.options.hooks.PreToolUse[0]!.hooks[0]!;
    const ac = new AbortController();
    const hookP = hook(
      {
        hook_event_name: "PreToolUse",
        session_id: this.sessionId,
        cwd: this.options.cwd,
        permission_mode: this.options.permissionMode,
        tool_name: name,
        tool_input: input,
        tool_use_id: id,
        ...(o.agentId ? { agent_id: o.agentId, agent_type: o.agentType ?? "general-purpose" } : {}),
      },
      id,
      { signal: ac.signal },
    );
    let timedOut = false;
    let out: Record<string, unknown>;
    if (o.hookTimeoutMs !== undefined) {
      const t = new Promise<"timeout">((r) => setTimeout(() => r("timeout"), o.hookTimeoutMs));
      const r = await Promise.race([hookP, t]);
      if (r === "timeout") {
        timedOut = true;
        ac.abort();
        this.userResult(id, true, "PreToolUse hook did not respond before its timeout. The tool call was not executed");
        return { id, allowed: false, timedOut, reason: "timeout" };
      }
      out = r;
    } else {
      out = await hookP;
    }
    const hso = out.hookSpecificOutput as { permissionDecision?: string; permissionDecisionReason?: string } | undefined;
    if (hso?.permissionDecision === "deny") {
      this.userResult(id, true, `PreToolUse:${name} hook error: ${hso.permissionDecisionReason}`);
      return { id, allowed: false, reason: hso.permissionDecisionReason ?? "" };
    }
    this.userResult(id, false, "ok");
    return { id, allowed: true };
  }

  private userResult(id: string, isError: boolean, content: string) {
    this.emit({ type: "user", message: { content: [{ type: "tool_result", tool_use_id: id, is_error: isError, content }] } });
  }

  emit(m: SdkMessage) {
    this.out.push(m);
  }

  get turnCount() {
    return this.turns;
  }

  async interrupt() {
    this.interrupted++;
    return undefined;
  }
  async setPermissionMode(m: PermissionMode) {
    this.modes.push(m);
  }
  close() {
    this.closed = true;
    this.out.end();
    this.release();
  }
  [Symbol.asyncIterator](): AsyncIterator<SdkMessage> {
    return this.out[Symbol.asyncIterator]();
  }
}

export class FakeSdk implements Sdk {
  readonly queries: FakeQuery[] = [];
  readonly sessions: SessionInfo[] = [];
  readonly renames: { id: string; title: string }[] = [];
  readonly forks: { id: string; title?: string; newId: string }[] = [];
  probeResult: ProbeResult = { account: { tokenSource: "none" } };
  probeError: Error | null = null;
  /** The script of every turn. Tests replace it. */
  turn: TurnFn = (ctx) => {
    ctx.say(`echo: ${ctx.text}`);
    ctx.result();
  };

  query(p: { prompt: AsyncIterable<SdkUserMessage>; options: QueryOptions }): SdkQuery {
    const q = new FakeQuery(p.options, p.prompt, (ctx) => this.turn(ctx));
    this.queries.push(q);
    return q;
  }
  get last(): FakeQuery {
    const q = this.queries[this.queries.length - 1];
    if (!q) throw new Error("no query yet");
    return q;
  }
  async listSessions(): Promise<SessionInfo[]> {
    return this.sessions;
  }
  async getSessionMessages(id: string, o: { limit?: number }): Promise<unknown[]> {
    const all = [{ type: "user", uuid: "u1", session_id: id }, { type: "assistant", uuid: "a1", session_id: id }];
    return o.limit ? all.slice(0, o.limit) : all;
  }
  async renameSession(id: string, title: string) {
    this.renames.push({ id, title });
  }
  async forkSession(id: string, o: { title?: string }) {
    const newId = randomUUID();
    this.forks.push({ id, newId, ...(o.title !== undefined ? { title: o.title } : {}) });
    return { sessionId: newId };
  }
  async probe(): Promise<ProbeResult> {
    if (this.probeError) throw this.probeError;
    return this.probeResult;
  }
}
