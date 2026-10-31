// A thin interface over the Claude Agent SDK. The runner only talks to this, so tests can
// inject a fake SDK (see test/fake-sdk.ts). The real implementation is in sdk-real.ts.

export type PermissionMode = "default" | "acceptEdits" | "bypassPermissions" | "plan" | "dontAsk" | "auto";
export const PERMISSION_MODES: readonly PermissionMode[] = ["default", "acceptEdits", "bypassPermissions", "plan", "dontAsk", "auto"];

/** Any SDK message; the runner reads only the fields it needs and ignores the rest. */
export interface SdkMessage {
  type: string;
  subtype?: string;
  [k: string]: unknown;
}

export interface SdkUserMessage {
  type: "user";
  message: { role: "user"; content: string };
  parent_tool_use_id: null;
  priority?: "now" | "next" | "later";
}

export interface PreToolUseInput {
  hook_event_name: "PreToolUse";
  session_id: string;
  cwd: string;
  permission_mode?: string;
  tool_name: string;
  tool_input: unknown;
  tool_use_id: string;
  agent_id?: string;
  agent_type?: string;
}

export type HookOutput = Record<string, unknown>;
export type HookFn = (input: PreToolUseInput, toolUseId: string | undefined, o: { signal: AbortSignal }) => Promise<HookOutput>;

export type CanUseToolResult =
  | { behavior: "allow"; updatedInput?: Record<string, unknown> }
  | { behavior: "deny"; message: string };
export type CanUseToolFn = (
  tool: string,
  input: Record<string, unknown>,
  o: { signal: AbortSignal; toolUseID?: string; agentID?: string },
) => Promise<CanUseToolResult>;

export interface QueryOptions {
  cwd: string;
  /** New session: the id chosen up front. */
  sessionId?: string;
  /** Idle session: continue it. */
  resume?: string;
  title?: string;
  settingSources: string[];
  /** Flag-layer settings: they win over the project's settings files. */
  settings: Record<string, unknown>;
  strictMcpConfig: boolean;
  mcpServers: Record<string, unknown>;
  allowDangerouslySkipPermissions: boolean;
  permissionMode: PermissionMode;
  env: Record<string, string>;
  disallowedTools: string[];
  abortController: AbortController;
  hooks: { PreToolUse: { matcher?: string; timeout: number; hooks: HookFn[] }[] };
  canUseTool: CanUseToolFn;
  stderr?: (data: string) => void;
}

export interface SdkQuery extends AsyncIterable<SdkMessage> {
  interrupt(): Promise<unknown>;
  setPermissionMode(mode: PermissionMode): Promise<void>;
  close(): void;
}

export interface SessionInfo {
  sessionId: string;
  summary: string;
  customTitle?: string;
  lastModified: number;
  createdAt?: number;
}

export interface ProbeResult {
  account?: { subscriptionType?: string; tokenSource?: string; apiKeySource?: string; apiProvider?: string };
  limits?: unknown;
}

export interface Sdk {
  query(p: { prompt: AsyncIterable<SdkUserMessage>; options: QueryOptions }): SdkQuery;
  listSessions(o: { dir: string }): Promise<SessionInfo[]>;
  getSessionMessages(id: string, o: { dir: string; limit?: number }): Promise<unknown[]>;
  renameSession(id: string, title: string, o: { dir: string }): Promise<void>;
  forkSession(id: string, o: { dir: string; title?: string }): Promise<{ sessionId: string }>;
  /** Zero-cost probe: no prompt is sent, no tokens are spent. */
  probe(o: { env: Record<string, string>; cwd: string; timeoutMs?: number }): Promise<ProbeResult>;
}
