// The real SDK behind the Sdk interface.
// probe() follows the zero-cost probe of T3 Code (MIT, Copyright (c) 2026 T3 Tools Inc.,
// upstream pingdotgg/t3code, apps/server/src/provider/ClaudeProvider.ts; see ../LICENSE-T3):
// a prompt that never yields, then initializationResult() and the usage call. Changes: Effect-TS
// removed; the account email is dropped.
import * as sdk from "@anthropic-ai/claude-agent-sdk";
import type { ProbeResult, Sdk, SdkMessage, SdkQuery } from "./sdk.js";

function waitForAbort(signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.resolve();
  return new Promise((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
}

export const realSdk: Sdk = {
  query({ prompt, options }) {
    // Our QueryOptions is a strict subset of the SDK's Options, with looser hook types.
    const q = sdk.query({ prompt: prompt as never, options: options as never });
    return q as unknown as SdkQuery & AsyncIterable<SdkMessage>;
  },
  async listSessions(o) {
    return (await sdk.listSessions({ dir: o.dir })) as never;
  },
  async getSessionMessages(id, o) {
    return (await sdk.getSessionMessages(id, { dir: o.dir, ...(o.limit !== undefined ? { limit: o.limit } : {}) })) as unknown[];
  },
  async renameSession(id, title, o) {
    await sdk.renameSession(id, title, { dir: o.dir });
  },
  async forkSession(id, o) {
    return sdk.forkSession(id, { dir: o.dir, ...(o.title !== undefined ? { title: o.title } : {}) });
  },
  async probe({ env, cwd, timeoutMs = 20000 }): Promise<ProbeResult> {
    const abort = new AbortController();
    const timer = setTimeout(() => abort.abort(), timeoutMs);
    try {
      const q = sdk.query({
        prompt: (async function* () {
          await waitForAbort(abort.signal);
        })() as never,
        options: {
          persistSession: false,
          abortController: abort,
          settingSources: [],
          settings: { disableAllHooks: true },
          allowedTools: [],
          mcpServers: {},
          strictMcpConfig: true,
          env: { ...env, ENABLE_CLAUDEAI_MCP_SERVERS: "false", CLAUDE_CODE_AUTO_CONNECT_IDE: "0", CLAUDE_CODE_IDE_SKIP_AUTO_INSTALL: "1" },
          cwd,
          stderr: () => {},
        } as never,
      });
      const init = await q.initializationResult();
      const a = (init.account ?? {}) as Record<string, string | undefined>;
      const out: ProbeResult = {
        account: { subscriptionType: a.subscriptionType, tokenSource: a.tokenSource, apiKeySource: a.apiKeySource, apiProvider: a.apiProvider },
      };
      try {
        const u = await q.usage_EXPERIMENTAL_MAY_CHANGE_DO_NOT_RELY_ON_THIS_API_YET({ skipBehaviors: true });
        if (u.rate_limits_available) out.limits = u.rate_limits;
      } catch {
        // no usage without a login; the account part above still answers "logged in?"
      }
      return out;
    } finally {
      clearTimeout(timer);
      if (!abort.signal.aborted) abort.abort();
    }
  },
};
