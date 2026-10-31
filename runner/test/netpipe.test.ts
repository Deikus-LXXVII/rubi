import assert from "node:assert/strict";
import { spawn, type ChildProcess } from "node:child_process";
import net from "node:net";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { after, describe, it } from "node:test";

const SCRIPT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../container/rubi-netpipe");
const T = { HELLO: 1, OPEN: 2, DATA: 3, CLOSE: 4, RESET: 5, ACK: 6 } as const;
const WINDOW = 262144;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const until = async (what: string, f: () => boolean, ms = 4000) => {
  for (let t = 0; t < ms; t += 5) {
    if (f()) return;
    await sleep(5);
  }
  throw new Error("timed out: " + what);
};

function frame(type: number, id: number, payload: Buffer = Buffer.alloc(0)): Buffer {
  const h = Buffer.alloc(9);
  h[0] = type;
  h.writeUInt32BE(id, 1);
  h.writeUInt32BE(payload.length, 5);
  return Buffer.concat([h, payload]);
}
const u32 = (n: number) => {
  const b = Buffer.alloc(4);
  b.writeUInt32BE(n);
  return b;
};

async function freePort(): Promise<number> {
  return new Promise((res) => {
    const s = net.createServer();
    s.listen(0, "127.0.0.1", () => {
      const p = (s.address() as net.AddressInfo).port;
      s.close(() => res(p));
    });
  });
}

interface Fr {
  type: number;
  id: number;
  payload: Buffer;
}

/** The Home side: parses what the forwarder writes to its stdout, writes frames to its stdin. */
class Hub {
  p: ChildProcess;
  frames: Fr[] = [];
  stderr = "";
  exit: Promise<number | null> = Promise.resolve(null);
  exited = false;
  proxyPort = 0;
  gatewayPort = 0;
  private buf = Buffer.alloc(0);

  static async start(): Promise<Hub> {
    const h = new Hub();
    h.proxyPort = await freePort();
    h.gatewayPort = await freePort();
    h.p = spawn(process.execPath, [SCRIPT, "--proxy-port", String(h.proxyPort), "--gateway-port", String(h.gatewayPort)], { stdio: ["pipe", "pipe", "pipe"] });
    h.p.stdout!.on("data", (d: Buffer) => {
      h.buf = Buffer.concat([h.buf, d]);
      while (h.buf.length >= 9 && h.buf.length >= 9 + h.buf.readUInt32BE(5)) {
        const n = h.buf.readUInt32BE(5);
        h.frames.push({ type: h.buf[0]!, id: h.buf.readUInt32BE(1), payload: h.buf.subarray(9, 9 + n) });
        h.buf = h.buf.subarray(9 + n);
      }
    });
    h.p.stderr!.on("data", (d: Buffer) => (h.stderr += d.toString()));
    h.p.stdin!.on("error", () => {});
    h.exit = new Promise((r) =>
      h.p.on("close", (c) => {
        h.exited = true;
        r(c);
      }),
    );
    all.push(h);
    return h;
  }
  private constructor() {
    this.p = undefined as unknown as ChildProcess;
  }
  hello = () => until("hello", () => this.frames.some((f) => f.type === T.HELLO));
  send = (type: number, id: number, payload?: Buffer) => this.p.stdin!.write(frame(type, id, payload));
  of = (type: number, id?: number) => this.frames.filter((f) => f.type === type && (id === undefined || f.id === id));
  data = (id: number) => Buffer.concat(this.of(T.DATA, id).map((f) => f.payload));
  acked = (id: number) => this.of(T.ACK, id).reduce((n, f) => n + f.payload.readUInt32BE(0), 0);
  /** Connects a client to a port and returns it with the stream id Home sees. */
  async client(port: number): Promise<{ c: net.Socket; id: number; got: Buffer[]; closed: () => boolean; ended: () => boolean; gone: () => boolean }> {
    const before = this.of(T.OPEN).length;
    const c = net.connect({ port, host: "127.0.0.1", allowHalfOpen: true });
    const got: Buffer[] = [];
    let closed = false, ended = false;
    c.on("data", (d) => got.push(d));
    c.on("close", () => (closed = true));
    c.on("end", () => (ended = true));
    c.on("error", () => {});
    await until("open", () => this.of(T.OPEN).length > before);
    return { c, id: this.of(T.OPEN)[before]!.id, got, closed: () => closed, ended: () => ended, gone: () => closed || ended };
  }
  async stop() {
    this.p.stdin!.end();
    if (!this.exited) await Promise.race([this.exit, sleep(2000)]);
    if (!this.exited) this.p.kill();
  }
}
const all: Hub[] = [];
after(async () => {
  for (const h of all) await h.stop();
});

describe("rubi-netpipe", () => {
  it("says HELLO (version 1) once both ports listen, and not before", async () => {
    const h = await Hub.start();
    await h.hello();
    assert.deepEqual(h.of(T.HELLO)[0], { type: T.HELLO, id: 0, payload: Buffer.from([1]) });
    // both ports accept now
    for (const port of [h.proxyPort, h.gatewayPort]) {
      const { c } = await h.client(port);
      c.destroy();
    }
  });

  it("opens a stream per connection, naming the service by port", async () => {
    const h = await Hub.start();
    await h.hello();
    const a = await h.client(h.proxyPort);
    const b = await h.client(h.gatewayPort);
    const c = await h.client(h.proxyPort);
    const opens = h.of(T.OPEN);
    assert.deepEqual(opens.map((f) => f.payload[0]), [1, 2, 1]);
    assert.equal(new Set([a.id, b.id, c.id]).size, 3);
    assert.ok(a.id > 0);
  });

  it("carries bytes both ways, binary safe", async () => {
    const h = await Hub.start();
    await h.hello();
    const { c, id, got } = await h.client(h.proxyPort);
    const all256 = Buffer.from(Array.from({ length: 1024 }, (_, i) => i & 255));
    c.write(all256);
    await until("data to home", () => h.data(id).length === 1024);
    assert.ok(h.data(id).equals(all256));
    h.send(T.DATA, id, all256);
    await until("data to client", () => Buffer.concat(got).length === 1024);
    assert.ok(Buffer.concat(got).equals(all256));
  });

  it("splits what the client writes into frames of at most 32 KiB", async () => {
    const h = await Hub.start();
    await h.hello();
    const { c, id } = await h.client(h.proxyPort);
    const big = Buffer.alloc(100_000, 7);
    c.write(big);
    await until("all", () => h.data(id).length === big.length);
    assert.ok(h.of(T.DATA, id).every((f) => f.payload.length <= 32768));
  });

  it("stops the client at the window until Home acknowledges, then goes on", async () => {
    const h = await Hub.start();
    await h.hello();
    const { c, id } = await h.client(h.proxyPort);
    const total = 1_000_000;
    const chunk = Buffer.alloc(total, 9);
    c.write(chunk);
    await until("window filled", () => h.data(id).length === WINDOW);
    await sleep(150);
    assert.equal(h.data(id).length, WINDOW, "never more than the window without an ACK");
    h.send(T.ACK, id, u32(100_000));
    await until("more after ack", () => h.data(id).length === WINDOW + 100_000);
    await sleep(100);
    assert.equal(h.data(id).length, WINDOW + 100_000);
    // acknowledge the rest in steps: everything arrives, intact and in order
    for (let i = 0; i < 100 && h.data(id).length < total; i++) {
      h.send(T.ACK, id, u32(Math.min(WINDOW, total - h.data(id).length)));
      await sleep(20);
    }
    assert.equal(h.data(id).length, total);
    assert.ok(h.data(id).equals(chunk));
  });

  it("acknowledges what it wrote to the client, and resets a stream that gets data after CLOSE", async () => {
    const h = await Hub.start();
    await h.hello();
    const { c, id, got } = await h.client(h.proxyPort);
    const part = Buffer.alloc(30_000, 1);
    for (let i = 0; i < 6; i++) h.send(T.DATA, id, part);
    await until("delivered", () => Buffer.concat(got).length === 180_000);
    await until("acked", () => h.acked(id) >= 150_000);
    assert.ok(h.acked(id) <= 180_000);
    // Data after Home's own CLOSE breaks the protocol for that stream: it is reset, the program lives on.
    const h2 = await Hub.start();
    await h2.hello();
    const v = await h2.client(h2.proxyPort);
    h2.send(T.CLOSE, v.id);
    h2.send(T.DATA, v.id, part);
    await until("reset", () => h2.of(T.RESET, v.id).length === 1);
    await until("socket closed", () => v.gone());
    assert.equal(h2.exited, false, "only the stream is reset");
  });

  it("half close: the client's FIN is a CLOSE, and the other direction goes on", async () => {
    const h = await Hub.start();
    await h.hello();
    const { c, id, got, ended } = await h.client(h.proxyPort);
    c.write("request");
    c.end();
    await until("close frame", () => h.of(T.CLOSE, id).length === 1);
    assert.equal(h.data(id).toString(), "request");
    h.send(T.DATA, id, Buffer.from("response"));
    await until("response", () => Buffer.concat(got).toString() === "response");
    assert.equal(ended(), false);
    h.send(T.CLOSE, id);
    await until("end", () => ended());
    assert.equal(h.of(T.RESET, id).length, 0, "a clean end needs no reset");
  });

  it("Home's CLOSE ends the client's read side only", async () => {
    const h = await Hub.start();
    await h.hello();
    const { c, id, ended } = await h.client(h.proxyPort);
    h.send(T.CLOSE, id);
    await until("fin", () => ended());
    c.write("still writing");
    await until("data", () => h.data(id).toString() === "still writing");
    c.end();
    await until("close", () => h.of(T.CLOSE, id).length === 1);
  });

  it("a destroyed client socket is a RESET; Home's RESET destroys the socket", async () => {
    const h = await Hub.start();
    await h.hello();
    const a = await h.client(h.proxyPort);
    a.c.resetAndDestroy(); // a connection reset (a crashed process)
    await until("reset", () => h.of(T.RESET, a.id).length === 1);
    const f = await h.client(h.proxyPort);
    f.c.destroy(); // a FIN: the client has nothing more to say, which is a half close
    await until("close", () => h.of(T.CLOSE, f.id).length === 1);
    assert.equal(h.of(T.RESET, f.id).length, 0);
    const b = await h.client(h.proxyPort);
    h.send(T.RESET, b.id);
    await until("closed", () => b.gone());
    await sleep(50);
    assert.equal(h.of(T.RESET, b.id).length, 0, "no reset is echoed");
  });

  it("refuses the 65th stream without sending OPEN, and a place frees up", async () => {
    const h = await Hub.start();
    await h.hello();
    const cs: Awaited<ReturnType<Hub["client"]>>[] = [];
    for (let i = 0; i < 64; i++) cs.push(await h.client(h.proxyPort));
    const extra = net.connect(h.proxyPort, "127.0.0.1");
    let closed = false;
    extra.on("close", () => (closed = true));
    extra.on("error", () => {});
    await until("refused", () => closed);
    assert.equal(h.of(T.OPEN).length, 64);
    cs[0]!.c.resetAndDestroy();
    await until("reset", () => h.of(T.RESET, cs[0]!.id).length === 1);
    const again = await h.client(h.proxyPort);
    assert.ok(again.id > 64);
  });

  it("carries many concurrent streams at once", async () => {
    const h = await Hub.start();
    await h.hello();
    const n = 50;
    const clients: Awaited<ReturnType<Hub["client"]>>[] = [];
    for (let i = 0; i < n; i++) clients.push(await h.client(i % 2 ? h.gatewayPort : h.proxyPort));
    const payloads = clients.map((_, i) => Buffer.alloc(100_000 + i, i));
    clients.forEach((cl, i) => cl.c.write(payloads[i]!));
    await until("all data", () => clients.every((cl, i) => h.data(cl.id).length === payloads[i]!.length), 10_000);
    clients.forEach((cl, i) => {
      assert.ok(h.data(cl.id).equals(payloads[i]!), "stream " + i);
      h.send(T.DATA, cl.id, h.data(cl.id).subarray(0, 20_000));
    });
    await until("echo", () => clients.every((cl) => Buffer.concat(cl.got).length === 20_000));
    clients.forEach((cl, i) => assert.ok(Buffer.concat(cl.got).equals(payloads[i]!.subarray(0, 20_000))));
  });

  it("ignores frames for streams that don't exist, and replies to an OPEN from Home with a RESET", async () => {
    const h = await Hub.start();
    await h.hello();
    h.send(T.DATA, 999, Buffer.from("x"));
    h.send(T.CLOSE, 999);
    h.send(T.RESET, 999);
    h.send(T.ACK, 999, u32(5));
    h.send(T.OPEN, 5, Buffer.from([1]));
    await until("reset", () => h.of(T.RESET, 5).length === 1);
    assert.equal(h.exited, false);
    const { c, id } = await h.client(h.proxyPort);
    c.write("ok");
    await until("still working", () => h.data(id).toString() === "ok");
  });

  describe("malformed frames end the program (exit 2, a line on stderr, sockets closed)", () => {
    const bad: Record<string, () => Buffer> = {
      "unknown type": () => frame(99, 1),
      "oversize length": () => {
        const f = frame(T.DATA, 1);
        f.writeUInt32BE(32769, 5);
        return f;
      },
      "huge length": () => {
        const f = frame(T.DATA, 1);
        f.writeUInt32BE(0xffffffff, 5);
        return f;
      },
      "data without a stream id": () => frame(T.DATA, 0, Buffer.from("x")),
      "ack of 3 bytes": () => frame(T.ACK, 1, Buffer.alloc(3)),
      "close with a payload": () => frame(T.CLOSE, 1, Buffer.from("x")),
    };
    for (const [name, mk] of Object.entries(bad)) {
      it(name, async () => {
        const h = await Hub.start();
        await h.hello();
        const cl = await h.client(h.proxyPort);
        h.p.stdin!.write(mk());
        assert.equal(await h.exit, 2);
        assert.match(h.stderr, /malformed frame/);
        await until("socket closed", () => cl.gone());
      });
    }
  });

  it("exits 0 and closes every socket when the channel closes", async () => {
    const h = await Hub.start();
    await h.hello();
    const cl = await h.client(h.proxyPort);
    h.p.stdin!.end();
    assert.equal(await h.exit, 0);
    await until("socket closed", () => cl.gone());
  });

  it("exits 1 when a port is taken", async () => {
    const taken = net.createServer();
    await new Promise<void>((r) => taken.listen(0, "127.0.0.1", () => r()));
    const port = (taken.address() as net.AddressInfo).port;
    const p = spawn(process.execPath, [SCRIPT, "--proxy-port", String(port), "--gateway-port", String(await freePort())], { stdio: ["pipe", "pipe", "pipe"] });
    let err = "";
    p.stderr.on("data", (d: Buffer) => (err += d));
    const code = await new Promise((r) => p.on("close", r));
    taken.close();
    assert.equal(code, 1);
    assert.match(err, /can't listen/);
  });

  it("listens on loopback only", async () => {
    const h = await Hub.start();
    await h.hello();
    // a connection to the machine's other addresses is refused (the server is bound to 127.0.0.1)
    const ifaces = Object.values((await import("node:os")).networkInterfaces()).flat().filter((i) => i && i.family === "IPv4" && !i.internal);
    for (const i of ifaces) {
      const r = await new Promise<string>((res) => {
        const s = net.connect({ host: i!.address, port: h.proxyPort, timeout: 500 });
        s.on("connect", () => (s.destroy(), res("connected")));
        s.on("error", (e: NodeJS.ErrnoException) => res(e.code ?? "error"));
        s.on("timeout", () => (s.destroy(), res("timeout")));
      });
      assert.notEqual(r, "connected");
    }
  });
});
