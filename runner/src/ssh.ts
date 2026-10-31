// The SSH relay inside the VM. The `ssh` wrapper (runner/container/rubi-ssh) connects to a unix socket served here;
// the runner turns its connection into an ssh.open request to Rubi Home and relays the stream (contract:
// docs/design/agent-runner.md, "SSH relay (R11)"). The runner never sees a host, a user or a key, and never logs a
// command or any payload.
//
// Wrapper <-> runner framing on the socket: 1 byte type, 4 bytes big-endian payload length, payload.
//   wrapper -> runner: OPEN (JSON {target, command, session_id?}), STDIN (bytes), EOF, ACK (uint32 n: bytes of
//                      output the wrapper has written to its stdout/stderr)
//   runner -> wrapper: ERR (utf8 text, the open failed), STDOUT, STDERR (bytes), EXIT (JSON {code, reason?})
import fs from "node:fs";
import net from "node:net";

export const SSH_WINDOW = 8 * 1024 * 1024; // bytes unacknowledged per direction (the Home contract)
export const SSH_CHUNK = 32 * 1024; // at most this much per ssh.data notification
export const MAX_FRAME = 128 * 1024; // the largest frame the runner accepts from the wrapper
const QUEUE_HIGH = 256 * 1024; // input waiting for Home's window: stop reading the socket above this
const QUEUE_LOW = 64 * 1024;
const TARGET_RE = /^[a-z0-9][a-z0-9-]{0,31}$/;
const MAX_COMMAND = 8192;

export const F = { OPEN: 1, STDIN: 2, EOF: 3, ACK: 4, ERR: 0x11, STDOUT: 0x12, STDERR: 0x13, EXIT: 0x14 } as const;

export function frame(type: number, payload: Buffer | string = Buffer.alloc(0)): Buffer {
  const p = typeof payload === "string" ? Buffer.from(payload) : payload;
  const h = Buffer.alloc(5);
  h[0] = type;
  h.writeUInt32BE(p.length, 1);
  return Buffer.concat([h, p]);
}

export interface SshDeps {
  request(method: string, params: Record<string, unknown>, o: { timeoutMs: number; onResult: (r: unknown) => void }): Promise<unknown>;
  notify(method: string, params: Record<string, unknown>): void;
  log(m: string): void;
  /** True when the id names a session this runner holds. */
  knownSession(id: string): boolean;
  /** The tool_use_id of the one running Bash call of the session that runs `ssh`, when there is exactly one. */
  toolUseId(sessionId: string): string | undefined;
  openTimeoutMs: number;
}

interface Conn {
  sock: net.Socket;
  buf: Buffer;
  opened: boolean; // OPEN frame seen
  gone: boolean; // the wrapper hung up
  stream?: string;
  sessionId?: string;
  queue: { data?: Buffer; eof?: boolean }[];
  queued: number;
  eofSent: boolean;
  inflight: number; // input bytes sent to Home and not acknowledged
  delivered: number; // output bytes handed to the wrapper and not yet acknowledged by it
  paused: boolean;
}

export interface SshServer {
  /** Cuts the streams of one session (interrupt, close) or, with no id, all of them; the sockets are closed. */
  closeStreams(sessionId?: string): void;
  close(): void;
  notification(method: string, p: Record<string, unknown>): void;
}

export function serveSsh(path: string, d: SshDeps): Promise<SshServer> {
  try {
    fs.unlinkSync(path); // a stale socket of an earlier runner
  } catch {
    // none
  }
  const byStream = new Map<string, Conn>();
  const conns = new Set<Conn>();

  const send = (c: Conn, f: Buffer) => {
    if (!c.sock.destroyed && c.sock.writable) c.sock.write(f);
  };
  const fail = (c: Conn, msg: string) => {
    send(c, frame(F.ERR, msg));
    c.sock.end();
  };
  const hangUp = (c: Conn) => {
    // Tell Home to drop the stream; its ssh.exit that follows finds no entry and is ignored.
    if (c.stream && byStream.get(c.stream) === c) {
      byStream.delete(c.stream);
      d.notify("ssh.close", { stream: c.stream });
    }
  };

  const pump = (c: Conn) => {
    const stream = c.stream;
    if (!stream || byStream.get(stream) !== c) return;
    while (c.queue.length > 0) {
      const it = c.queue[0]!;
      if (it.eof) {
        c.queue.shift();
        c.eofSent = true;
        d.notify("ssh.data", { stream, fd: 0, eof: true });
        continue;
      }
      const room = SSH_WINDOW - c.inflight;
      if (room <= 0) break;
      const n = Math.min(it.data!.length, room, SSH_CHUNK);
      const piece = it.data!.subarray(0, n);
      if (n === it.data!.length) c.queue.shift();
      else it.data = it.data!.subarray(n);
      c.queued -= n;
      c.inflight += n;
      d.notify("ssh.data", { stream, fd: 0, b64: piece.toString("base64") });
    }
    if (c.paused && c.queued <= QUEUE_LOW && !c.sock.destroyed) {
      c.paused = false;
      c.sock.resume();
    }
  };

  const onFrame = (c: Conn, type: number, p: Buffer) => {
    switch (type) {
      case F.OPEN:
        if (c.opened) return fail(c, "protocol error");
        c.opened = true;
        return open(c, p);
      case F.STDIN:
        if (!c.opened || c.eofSent || c.queue.some((q) => q.eof)) return;
        c.queue.push({ data: Buffer.from(p) });
        c.queued += p.length;
        if (c.queued > QUEUE_HIGH && !c.paused) {
          c.paused = true;
          c.sock.pause();
        }
        return pump(c);
      case F.EOF:
        if (!c.opened || c.queue.some((q) => q.eof) || c.eofSent) return;
        c.queue.push({ eof: true });
        return pump(c);
      case F.ACK: {
        if (p.length !== 4 || !c.stream) return;
        const n = Math.min(p.readUInt32BE(0), c.delivered); // a wrapper can't ack what it was not given
        if (n <= 0) return;
        c.delivered -= n;
        d.notify("ssh.ack", { stream: c.stream, n });
        return;
      }
      default:
        return fail(c, "protocol error");
    }
  };

  const open = (c: Conn, p: Buffer) => {
    let o: { target?: unknown; command?: unknown; session_id?: unknown };
    try {
      o = JSON.parse(p.toString("utf8"));
    } catch {
      return fail(c, "protocol error");
    }
    if (typeof o.target !== "string" || !TARGET_RE.test(o.target)) return fail(c, "the target is a registered name (lowercase letters, digits, dashes)");
    if (typeof o.command !== "string" || o.command === "" || Buffer.byteLength(o.command) > MAX_COMMAND) return fail(c, "give a command (up to 8192 bytes); there is no interactive shell");
    const sid = typeof o.session_id === "string" && d.knownSession(o.session_id) ? o.session_id : undefined;
    c.sessionId = sid;
    const tool = sid ? d.toolUseId(sid) : undefined;
    d.log(`ssh.open: target ${o.target}${sid ? ` session ${sid}` : ""}`); // never the command
    d.request(
      "ssh.open",
      { target: o.target, command: o.command, session_id: sid ?? "", ...(tool ? { tool_use_id: tool } : {}) },
      {
        timeoutMs: d.openTimeoutMs,
        // synchronous: Home's first notification about the stream can follow the answer in the same chunk
        onResult: (r) => {
          const id = (r as { stream?: unknown } | null)?.stream;
          if (typeof id !== "string" || id === "") return;
          c.stream = id;
          byStream.set(id, c);
          if (c.gone) {
            hangUp(c);
            return;
          }
          pump(c);
        },
      },
    ).then(
      (r) => {
        if (typeof (r as { stream?: unknown } | null)?.stream !== "string") fail(c, "Rubi Home gave no stream");
      },
      (e) => {
        d.log(`ssh.open failed: ${e instanceof Error ? e.message : String(e)}`);
        fail(c, e instanceof Error && e.message ? e.message : "Rubi Home refused the connection");
      },
    );
  };

  const server = net.createServer((sock) => {
    const c: Conn = { sock, buf: Buffer.alloc(0), opened: false, gone: false, queue: [], queued: 0, eofSent: false, inflight: 0, delivered: 0, paused: false };
    conns.add(c);
    sock.on("data", (chunk: Buffer) => {
      c.buf = c.buf.length === 0 ? chunk : Buffer.concat([c.buf, chunk]);
      while (c.buf.length >= 5 && !sock.destroyed) {
        const len = c.buf.readUInt32BE(1);
        if (len > MAX_FRAME) {
          sock.destroy();
          return;
        }
        if (c.buf.length < 5 + len) break;
        const type = c.buf[0]!;
        const payload = c.buf.subarray(5, 5 + len);
        c.buf = c.buf.subarray(5 + len);
        onFrame(c, type, payload);
      }
    });
    const gone = () => {
      if (c.gone) return;
      c.gone = true;
      conns.delete(c);
      hangUp(c);
    };
    sock.on("close", gone);
    sock.on("error", () => {
      sock.destroy();
    });
  });

  const notification = (method: string, p: Record<string, unknown>) => {
    const stream = typeof p.stream === "string" ? p.stream : "";
    const c = byStream.get(stream);
    if (!c) return; // a stream we hung up, or a stranger
    switch (method) {
      case "ssh.data": {
        if ((p.fd !== 1 && p.fd !== 2) || typeof p.b64 !== "string") return;
        const b = Buffer.from(p.b64, "base64");
        if (b.length === 0) return;
        c.delivered += b.length;
        if (c.delivered > SSH_WINDOW + SSH_CHUNK) {
          d.log("ssh: Home sent more than the window; cutting the stream");
          hangUp(c);
          c.sock.destroy();
          return;
        }
        send(c, frame(p.fd === 1 ? F.STDOUT : F.STDERR, b));
        return;
      }
      case "ssh.ack": {
        const n = typeof p.n === "number" && Number.isFinite(p.n) ? Math.floor(p.n) : 0;
        if (n <= 0) return;
        c.inflight = Math.max(0, c.inflight - n);
        pump(c);
        return;
      }
      case "ssh.exit": {
        byStream.delete(stream);
        const code = typeof p.code === "number" && Number.isInteger(p.code) && p.code >= 0 && p.code <= 255 ? p.code : 255;
        const reason = typeof p.reason === "string" ? p.reason : undefined;
        d.log(`ssh.exit: code ${code}${reason ? ` (${reason})` : ""}`);
        send(c, frame(F.EXIT, JSON.stringify({ code, ...(reason ? { reason } : {}) })));
        c.sock.end();
        return;
      }
      default:
        return;
    }
  };

  const closeStreams = (sessionId?: string) => {
    for (const c of [...conns]) {
      if (sessionId !== undefined && c.sessionId !== sessionId) continue;
      hangUp(c);
      c.sock.end(frame(F.ERR, "the connection was closed by Rubi"));
      setTimeout(() => c.sock.destroy(), 1000).unref();
    }
  };

  return new Promise((resolve, reject) => {
    server.once("error", reject);
    const old = process.umask(0o177); // created 0600
    server.listen(path, () => {
      process.umask(old);
      try {
        fs.chmodSync(path, 0o600);
      } catch {
        // best effort; umask already did it
      }
      resolve({
        notification,
        closeStreams,
        close: () => {
          closeStreams();
          server.close();
          try {
            fs.unlinkSync(path);
          } catch {
            // gone
          }
        },
      });
    });
  });
}
