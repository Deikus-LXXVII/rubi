// Line-delimited JSON-RPC 2.0 over a pair of byte streams (the runner's stdin/stdout).
// One JSON object per line. A line is never longer than maxLineBytes; an over-long or
// malformed line is answered with an error (id null) and skipped, the stream goes on.
import type { Readable, Writable } from "node:stream";

export const DEFAULT_MAX_LINE_BYTES = 4 * 1024 * 1024;

export const ERR = {
  parse: -32700,
  invalidRequest: -32600,
  methodNotFound: -32601,
  invalidParams: -32602,
  internal: -32603,
  // implementation-defined range
  notReady: -32001, // request before the hello handshake finished
  timeout: -32002, // our own request got no answer in time
  closed: -32003, // the peer went away / the request was cancelled
  sessionNotFound: -32004,
  lineTooLong: -32005,
} as const;

export class RpcError extends Error {
  constructor(
    readonly code: number,
    message: string,
    readonly data?: unknown,
  ) {
    super(message);
    this.name = "RpcError";
  }
}

export type Json = null | boolean | number | string | Json[] | { [k: string]: Json | undefined };
export type RequestHandler = (method: string, params: Record<string, unknown>) => Promise<unknown>;

export interface PeerOptions {
  maxLineBytes?: number;
  onRequest: RequestHandler;
  onNotification?: (method: string, params: Record<string, unknown>) => void;
  /** Called once when the input stream ends or errors. */
  onClose?: () => void;
  log?: (msg: string) => void;
}

interface Pending {
  resolve: (v: unknown) => void;
  reject: (e: unknown) => void;
  timer?: NodeJS.Timeout;
  cleanup?: () => void;
}

export interface RequestOptions {
  timeoutMs?: number;
  signal?: AbortSignal;
}

export class Peer {
  private readonly maxLineBytes: number;
  private chunks: Buffer[] = [];
  private len = 0;
  private discarding = false;
  private nextId = 1;
  private readonly pending = new Map<number | string, Pending>();
  private closed = false;

  constructor(
    input: Readable,
    private readonly output: Writable,
    private readonly opts: PeerOptions,
  ) {
    this.maxLineBytes = opts.maxLineBytes ?? DEFAULT_MAX_LINE_BYTES;
    input.on("data", (c: Buffer | string) => this.onData(typeof c === "string" ? Buffer.from(c) : c));
    input.on("end", () => this.shutdown());
    input.on("close", () => this.shutdown());
    input.on("error", () => this.shutdown());
    output.on("error", () => {}); // EPIPE etc: the input side ending tells us the peer is gone
  }

  private log(m: string) {
    this.opts.log?.(m);
  }

  private shutdown() {
    if (this.closed) return;
    this.closed = true;
    for (const [id, p] of this.pending) {
      this.pending.delete(id);
      if (p.timer) clearTimeout(p.timer);
      p.cleanup?.();
      p.reject(new RpcError(ERR.closed, "connection closed"));
    }
    this.opts.onClose?.();
  }

  get isClosed(): boolean {
    return this.closed;
  }

  private onData(chunk: Buffer) {
    let start = 0;
    for (;;) {
      const nl = chunk.indexOf(10, start);
      if (nl === -1) {
        this.append(chunk.subarray(start));
        return;
      }
      this.append(chunk.subarray(start, nl));
      this.endLine();
      start = nl + 1;
    }
  }

  private append(b: Buffer) {
    if (this.discarding || b.length === 0) return;
    this.chunks.push(b);
    this.len += b.length;
    if (this.len > this.maxLineBytes) {
      this.chunks = [];
      this.len = 0;
      this.discarding = true; // drop everything up to the next newline
    }
  }

  private endLine() {
    if (this.discarding) {
      this.discarding = false;
      this.writeRaw({
        jsonrpc: "2.0",
        id: null,
        error: { code: ERR.lineTooLong, message: `line exceeds ${this.maxLineBytes} bytes; skipped` },
      });
      return;
    }
    const line = Buffer.concat(this.chunks).toString("utf8").trim();
    this.chunks = [];
    this.len = 0;
    if (line === "") return;
    this.handleLine(line);
  }

  private handleLine(line: string) {
    let msg: unknown;
    try {
      msg = JSON.parse(line);
    } catch {
      this.writeRaw({ jsonrpc: "2.0", id: null, error: { code: ERR.parse, message: "parse error" } });
      return;
    }
    if (typeof msg !== "object" || msg === null || Array.isArray(msg)) {
      this.writeRaw({ jsonrpc: "2.0", id: null, error: { code: ERR.invalidRequest, message: "a message must be a JSON object" } });
      return;
    }
    const m = msg as Record<string, unknown>;
    if (m.jsonrpc !== "2.0") {
      this.writeRaw({ jsonrpc: "2.0", id: idOrNull(m.id), error: { code: ERR.invalidRequest, message: 'jsonrpc must be "2.0"' } });
      return;
    }
    if (typeof m.method === "string") {
      const params = m.params === undefined ? {} : m.params;
      const hasId = "id" in m && m.id !== undefined;
      if (typeof params !== "object" || params === null || Array.isArray(params)) {
        if (hasId) this.writeRaw({ jsonrpc: "2.0", id: idOrNull(m.id), error: { code: ERR.invalidParams, message: "params must be an object" } });
        return;
      }
      if (!hasId) {
        try {
          this.opts.onNotification?.(m.method, params as Record<string, unknown>);
        } catch (e) {
          this.log(`notification handler failed: ${errMsg(e)}`);
        }
        return;
      }
      if (typeof m.id !== "string" && typeof m.id !== "number") {
        this.writeRaw({ jsonrpc: "2.0", id: null, error: { code: ERR.invalidRequest, message: "id must be a string or number" } });
        return;
      }
      void this.dispatch(m.id, m.method, params as Record<string, unknown>);
      return;
    }
    if ("id" in m && ("result" in m || "error" in m)) {
      const p = typeof m.id === "string" || typeof m.id === "number" ? this.pending.get(m.id) : undefined;
      if (!p) {
        this.log("response for an unknown or already finished request ignored");
        return;
      }
      this.pending.delete(m.id as number | string);
      if (p.timer) clearTimeout(p.timer);
      p.cleanup?.();
      if (m.error && typeof m.error === "object") {
        const e = m.error as Record<string, unknown>;
        p.reject(new RpcError(typeof e.code === "number" ? e.code : ERR.internal, typeof e.message === "string" ? e.message : "error", e.data));
      } else {
        p.resolve(m.result);
      }
      return;
    }
    this.writeRaw({ jsonrpc: "2.0", id: idOrNull(m.id), error: { code: ERR.invalidRequest, message: "not a request, notification or response" } });
  }

  private async dispatch(id: string | number, method: string, params: Record<string, unknown>) {
    try {
      const result = await this.opts.onRequest(method, params);
      this.writeRaw({ jsonrpc: "2.0", id, result: result === undefined ? null : result });
    } catch (e) {
      if (e instanceof RpcError) {
        this.writeRaw({ jsonrpc: "2.0", id, error: { code: e.code, message: e.message, ...(e.data !== undefined ? { data: e.data } : {}) } });
      } else {
        this.log(`handler for ${method} threw: ${errMsg(e)}`);
        this.writeRaw({ jsonrpc: "2.0", id, error: { code: ERR.internal, message: "internal error" } });
      }
    }
  }

  request(method: string, params: Record<string, unknown>, ro: RequestOptions = {}): Promise<unknown> {
    if (this.closed) return Promise.reject(new RpcError(ERR.closed, "connection closed"));
    if (ro.signal?.aborted) return Promise.reject(new RpcError(ERR.closed, "cancelled"));
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      const p: Pending = { resolve, reject };
      if (ro.timeoutMs !== undefined) {
        p.timer = setTimeout(() => {
          this.pending.delete(id);
          p.cleanup?.();
          reject(new RpcError(ERR.timeout, `no answer to ${method} in ${ro.timeoutMs} ms`));
        }, ro.timeoutMs);
      }
      if (ro.signal) {
        const onAbort = () => {
          if (!this.pending.delete(id)) return;
          if (p.timer) clearTimeout(p.timer);
          reject(new RpcError(ERR.closed, "cancelled"));
        };
        ro.signal.addEventListener("abort", onAbort, { once: true });
        p.cleanup = () => ro.signal?.removeEventListener("abort", onAbort);
      }
      this.pending.set(id, p);
      this.writeRaw({ jsonrpc: "2.0", id, method, params });
    });
  }

  notify(method: string, params: Record<string, unknown>) {
    this.writeRaw({ jsonrpc: "2.0", method, params });
  }

  private writeRaw(msg: unknown) {
    if (this.output.destroyed || this.output.writableEnded) return;
    try {
      this.output.write(JSON.stringify(msg) + "\n");
    } catch (e) {
      this.log(`write failed: ${errMsg(e)}`);
    }
  }
}

function idOrNull(id: unknown): string | number | null {
  return typeof id === "string" || typeof id === "number" ? id : null;
}

export function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
