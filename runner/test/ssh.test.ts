import assert from "node:assert/strict";
import { spawn, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, it } from "node:test";
import { Home, sent, tick } from "./harness.js";
import { F, frame, SSH_WINDOW } from "../src/ssh.js";

const WRAPPER = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../container/rubi-ssh");
const tmpSock = () => path.join(fs.mkdtempSync(path.join(os.tmpdir(), "rs-")), "ssh.sock");
const b64 = (s: string | Buffer) => Buffer.from(s).toString("base64");
const until = async (what: string, f: () => boolean, ms = 3000) => {
  for (let t = 0; t < ms; t += 10) {
    if (f()) return;
    await tick(10);
  }
  throw new Error("timed out: " + what);
};

/** Home has answered ssh.open (it sends nothing about the stream before that). */
const opened = (h: Home) => h.waitFor((l) => l.from === "home" && typeof l.msg.result?.stream === "string");

interface Wrapped {
  p: ChildProcess;
  out: Buffer[];
  err: string;
  done: Promise<number | null>;
}
function wrapper(sock: string, args: string[], env: Record<string, string> = {}): Wrapped {
  const p = spawn(process.execPath, [WRAPPER, ...args], { env: { PATH: process.env.PATH ?? "", RUBI_SSH_SOCK: sock, ...env }, stdio: ["pipe", "pipe", "pipe"] });
  const w: Wrapped = { p, out: [], err: "", done: new Promise((r) => p.on("close", (c) => r(c))) };
  p.stdout!.on("data", (d: Buffer) => w.out.push(d));
  p.stderr!.on("data", (d: Buffer) => (w.err += d.toString()));
  return w;
}
const stdout = (w: Wrapped) => Buffer.concat(w.out).toString();

/** A raw client of the runner's socket, as a wrapper would be. */
function raw(sockPath: string) {
  const c = net.connect(sockPath);
  const frames: { type: number; payload: Buffer }[] = [];
  let buf = Buffer.alloc(0);
  c.on("data", (d: Buffer) => {
    buf = Buffer.concat([buf, d]);
    while (buf.length >= 5 && buf.length >= 5 + buf.readUInt32BE(1)) {
      const n = buf.readUInt32BE(1);
      frames.push({ type: buf[0]!, payload: buf.subarray(5, 5 + n) });
      buf = buf.subarray(5 + n);
    }
  });
  c.on("error", () => {});
  return { c, frames, open: (o: object) => c.write(frame(F.OPEN, JSON.stringify(o))) };
}

describe("ssh wrapper argument parser", () => {
  const refused = async (args: string[], re: RegExp) => {
    const w = wrapper("/nonexistent/sock", args);
    assert.equal(await w.done, 2, `ssh ${args.join(" ")}`);
    assert.match(w.err, re);
    assert.equal(stdout(w), "");
  };
  it("refuses options before the target", async () => {
    for (const o of ["-o", "-i", "-L", "-R", "-D", "-A", "-W", "-J", "-p", "-t", "-v", "--"]) await refused([o, "web-1", "ls"], /options are not supported/);
    await refused(["-oProxyCommand=x", "web-1", "ls"], /options are not supported/);
  });
  it("refuses user@host and anything that is not a target name", async () => {
    await refused(["root@web-1", "ls"], /user@host/);
    await refused(["web-1@example.com", "ls"], /user@host/);
    await refused(["Web_1", "ls"], /not a target name/);
    await refused(["web.example.com", "ls"], /not a target name/);
    await refused(["a".repeat(33), "ls"], /not a target name/);
  });
  it("needs a command and refuses no arguments", async () => {
    await refused(["web-1"], /command is required/);
    await refused(["web-1", " "], /command is required/);
    await refused([], /usage/);
  });
  it("says so when the relay is not there", async () => {
    const w = wrapper("/nonexistent/sock", ["web-1", "ls"]);
    assert.equal(await w.done, 255);
    assert.match(w.err, /relay isn't available/);
  });
});

describe("ssh relay", () => {
  it("serves a socket with mode 0600 and removes it on exit", async () => {
    const sock = tmpSock();
    const h = await new Home({ sshSock: sock }).up();
    assert.equal(fs.statSync(sock).mode & 0o777, 0o600);
    h.closeStdin();
    await tick(30);
    assert.equal(fs.existsSync(sock), false);
  });

  it("opens a stream, relays data both ways with eof, and exits with the remote code", async () => {
    const sock = tmpSock();
    const h = await new Home({ sshSock: sock }).up();
    const w = wrapper(sock, ["web-1", "cat", "-n", "/etc/x"]);
    await opened(h);
    assert.deepEqual(h.sshOpens, [{ target: "web-1", command: "cat -n /etc/x", session_id: "" }]);
    w.p.stdin!.write("hello ");
    w.p.stdin!.end("world");
    await until("input and eof", () => sent(h, "ssh.data").some((d) => d.eof === true));
    const input = sent(h, "ssh.data").filter((d) => d.fd === 0 && d.b64).map((d) => Buffer.from(d.b64, "base64").toString()).join("");
    assert.equal(input, "hello world");
    h.sendRaw({ jsonrpc: "2.0", method: "ssh.data", params: { stream: "s1", fd: 1, b64: b64("out\n") } });
    h.sendRaw({ jsonrpc: "2.0", method: "ssh.data", params: { stream: "s1", fd: 2, b64: b64("err\n") } });
    h.sendRaw({ jsonrpc: "2.0", method: "ssh.exit", params: { stream: "s1", code: 7 } });
    assert.equal(await w.done, 7);
    assert.equal(stdout(w), "out\n");
    assert.equal(w.err, "err\n");
    // the output was acknowledged to Home as the wrapper wrote it
    await until("acks", () => sent(h, "ssh.ack").reduce((n, a) => n + a.n, 0) === 8);
    assert.ok(sent(h, "ssh.ack").every((a) => a.stream === "s1"));
    assert.equal(sent(h, "ssh.close").length, 0);
    h.closeStdin();
  });

  it("answers and notifies in one chunk: the first output is not lost", async () => {
    const sock = tmpSock();
    const h = await new Home({ sshSock: sock }).up();
    h.sshOpenHandler = (p) => {
      // Home writes the answer and the first notification back to back
      setImmediate(() => h.sendRaw({ jsonrpc: "2.0", method: "ssh.data", params: { stream: "s1", fd: 1, b64: b64("early\n") } }));
      return { stream: "s1" };
    };
    const w = wrapper(sock, ["web-1", "echo early"]);
    await h.waitFor((l) => l.from === "home" && l.msg.method === "ssh.data");
    h.sendRaw({ jsonrpc: "2.0", method: "ssh.exit", params: { stream: "s1", code: 0 } });
    assert.equal(await w.done, 0);
    assert.equal(stdout(w), "early\n");
    h.closeStdin();
  });

  it("maps an exit with a reason (code -1) to 255 and prints the reason", async () => {
    const sock = tmpSock();
    const h = await new Home({ sshSock: sock }).up();
    const w = wrapper(sock, ["web-1", "sleep 99"]);
    await opened(h);
    h.sendRaw({ jsonrpc: "2.0", method: "ssh.exit", params: { stream: "s1", code: -1, reason: "idle timeout" } });
    assert.equal(await w.done, 255);
    assert.match(w.err, /idle timeout/);
    h.closeStdin();
  });

  it("shows Home's refusal and exits 255", async () => {
    const sock = tmpSock();
    const h = await new Home({ sshSock: sock }).up();
    h.sshOpenHandler = () => {
      throw new Error("this SSH target isn't available to this role");
    };
    const w = wrapper(sock, ["web-1", "ls"]);
    assert.equal(await w.done, 255);
    assert.match(w.err, /this SSH target isn't available to this role/);
    assert.equal(stdout(w), "");
    h.closeStdin();
  });

  it("sends ssh.close when the wrapper goes away, and ignores what Home sends after", async () => {
    const sock = tmpSock();
    const h = await new Home({ sshSock: sock }).up();
    const w = wrapper(sock, ["web-1", "tail -f /var/log/x"]);
    await opened(h);
    await tick(30);
    w.p.kill("SIGKILL");
    await w.done;
    await until("ssh.close", () => sent(h, "ssh.close").length === 1);
    assert.deepEqual(sent(h, "ssh.close"), [{ stream: "s1" }]);
    h.sendRaw({ jsonrpc: "2.0", method: "ssh.data", params: { stream: "s1", fd: 1, b64: b64("late") } });
    h.sendRaw({ jsonrpc: "2.0", method: "ssh.exit", params: { stream: "s1", code: -1, reason: "closed by the runner" } });
    await tick(30);
    assert.equal(sent(h, "ssh.ack").length, 0);
    assert.equal(sent(h, "ssh.close").length, 1);
    h.closeStdin();
  });

  it("closes the stream when the wrapper goes away while Home is still deciding", async () => {
    const sock = tmpSock();
    const h = await new Home({ sshSock: sock }).up();
    let release!: () => void;
    h.sshOpenHandler = () => new Promise((r) => (release = () => r({ stream: "s9" })));
    const w = wrapper(sock, ["web-1", "ls"]);
    await h.waitFor((l) => l.from === "runner" && l.msg.method === "ssh.open");
    w.p.kill("SIGKILL");
    await w.done;
    await tick(30);
    release();
    await until("ssh.close", () => sent(h, "ssh.close").length === 1);
    assert.deepEqual(sent(h, "ssh.close"), [{ stream: "s9" }]);
    h.closeStdin();
  });

  it("holds input back at the 8 MiB window until Home acknowledges", async () => {
    const sock = tmpSock();
    const h = await new Home({ sshSock: sock }).up();
    const r = raw(sock);
    r.open({ target: "web-1", command: "cat" });
    await opened(h);
    const chunk = Buffer.alloc(64 * 1024, 0x61);
    const total = SSH_WINDOW + 1024 * 1024;
    const sentBytes = () => sent(h, "ssh.data").reduce((n, d) => n + (d.b64 ? Buffer.from(d.b64, "base64").length : 0), 0);
    for (let n = 0; n < total; n += chunk.length) r.c.write(frame(F.STDIN, chunk));
    r.c.write(frame(F.EOF));
    await until("window filled", () => sentBytes() === SSH_WINDOW);
    await tick(100);
    assert.equal(sentBytes(), SSH_WINDOW, "never more than the window unacknowledged");
    assert.equal(sent(h, "ssh.data").some((d) => d.eof), false, "eof waits behind the data");
    h.sendRaw({ jsonrpc: "2.0", method: "ssh.ack", params: { stream: "s1", n: SSH_WINDOW } });
    await until("rest and eof", () => sentBytes() === total && sent(h, "ssh.data").some((d) => d.eof));
    assert.ok(sent(h, "ssh.data").every((d) => !d.b64 || Buffer.from(d.b64, "base64").length <= 32 * 1024));
    r.c.destroy();
    h.closeStdin();
  });

  it("acknowledges output to Home only as the wrapper consumes it", async () => {
    const sock = tmpSock();
    const h = await new Home({ sshSock: sock }).up();
    const r = raw(sock);
    r.open({ target: "web-1", command: "yes" });
    await opened(h);
    h.sendRaw({ jsonrpc: "2.0", method: "ssh.data", params: { stream: "s1", fd: 1, b64: b64("12345") } });
    await until("frame", () => r.frames.length === 1);
    assert.equal(r.frames[0]!.type, F.STDOUT);
    await tick(30);
    assert.equal(sent(h, "ssh.ack").length, 0, "nothing acked while the wrapper has not consumed it");
    const n = Buffer.alloc(4);
    n.writeUInt32BE(3);
    r.c.write(frame(F.ACK, n));
    await until("ack", () => sent(h, "ssh.ack").length === 1);
    assert.deepEqual(sent(h, "ssh.ack")[0], { stream: "s1", n: 3 });
    n.writeUInt32BE(1000); // more than it was given: clamped to what is outstanding
    r.c.write(frame(F.ACK, n));
    await until("ack 2", () => sent(h, "ssh.ack").length === 2);
    assert.equal(sent(h, "ssh.ack")[1]!.n, 2);
    r.c.destroy();
    h.closeStdin();
  });

  it("cuts a stream whose Home sends more than the window", async () => {
    const sock = tmpSock();
    const h = await new Home({ sshSock: sock }).up();
    const r = raw(sock);
    r.open({ target: "web-1", command: "yes" });
    await opened(h);
    const big = b64(Buffer.alloc(32 * 1024, 1));
    for (let i = 0; i < 8 * 32 + 3; i++) h.sendRaw({ jsonrpc: "2.0", method: "ssh.data", params: { stream: "s1", fd: 1, b64: big } });
    await until("ssh.close", () => sent(h, "ssh.close").length === 1);
    r.c.destroy();
    h.closeStdin();
  });

  it("refuses a bad open (target, empty command) without asking Home", async () => {
    const sock = tmpSock();
    const h = await new Home({ sshSock: sock }).up();
    for (const o of [{ target: "root@x", command: "ls" }, { target: "web-1", command: "" }, { target: "web-1" }, { target: "-oFoo", command: "ls" }]) {
      const r = raw(sock);
      r.open(o);
      await until("err frame", () => r.frames.some((f) => f.type === F.ERR));
      r.c.destroy();
    }
    assert.equal(h.sshOpens.length, 0);
    h.closeStdin();
  });

  it("names the session and the one ssh Bash call when they are known", async () => {
    const sock = tmpSock();
    const h = await new Home({ sshSock: sock }).up();
    let sid = "";
    let release!: () => void;
    const hold = new Promise<void>((r) => (release = r));
    h.sdk.turn = async (ctx) => {
      await ctx.tool("Bash", { command: "ssh web-1 uptime" }, { holdResult: hold });
      ctx.result();
    };
    const started = await h.ok("session.start", { prompt: "go", mode: "bypassPermissions" });
    sid = started.id;
    await until("the call was asked about", () => h.asks.length === 1);
    const env = h.sdk.queries[0]!.options.env!;
    assert.equal(env.RUBI_SSH_SOCK, sock);
    assert.equal(env.RUBI_SESSION_ID, sid);
    const w = wrapper(sock, ["web-1", "uptime"], { RUBI_SESSION_ID: sid });
    await opened(h);
    assert.equal(h.sshOpens[0]!.session_id, sid);
    assert.equal(h.sshOpens[0]!.tool_use_id, h.asks[0]!.tool_use_id);
    h.sendRaw({ jsonrpc: "2.0", method: "ssh.exit", params: { stream: "s1", code: 0 } });
    assert.equal(await w.done, 0);
    // an unknown session id is not passed on
    const w2 = wrapper(sock, ["web-1", "uptime"], { RUBI_SESSION_ID: "00000000-0000-4000-8000-000000000000" });
    await until("second open", () => h.sshOpens.length === 2);
    assert.equal(h.sshOpens[1]!.session_id, "");
    assert.equal(h.sshOpens[1]!.tool_use_id, undefined);
    w2.p.kill("SIGKILL");
    release(); // the call ends: it is no longer the caller of anything
    await h.waitEvent("result");
    const w3 = wrapper(sock, ["web-1", "uptime"], { RUBI_SESSION_ID: sid });
    await until("third open", () => h.sshOpens.length === 3);
    assert.equal(h.sshOpens[2]!.tool_use_id, undefined);
    w3.p.kill("SIGKILL");
    h.closeStdin();
  });

  it("cuts the session's streams on interrupt and on close", async () => {
    for (const how of ["session.interrupt", "session.close"]) {
      const sock = tmpSock();
      const h = await new Home({ sshSock: sock }).up();
      h.sdk.turn = async () => {
        await new Promise(() => {});
      };
      const { id } = await h.ok("session.start", { prompt: "go", mode: "bypassPermissions" });
      const w = wrapper(sock, ["web-1", "sleep 999"], { RUBI_SESSION_ID: id });
      await opened(h);
      await h.ok(how, { id });
      await until("ssh.close", () => sent(h, "ssh.close").length === 1);
      assert.equal(await w.done, 255);
      assert.match(w.err, /closed by Rubi/);
      h.closeStdin();
    }
  });

  it("cuts every stream and removes the socket when the runner exits", async () => {
    const sock = tmpSock();
    const h = await new Home({ sshSock: sock }).up();
    const w = wrapper(sock, ["web-1", "sleep 999"]);
    await opened(h);
    h.closeStdin();
    assert.equal(await w.done, 255);
    assert.deepEqual(sent(h, "ssh.close"), [{ stream: "s1" }]);
    assert.equal(fs.existsSync(sock), false);
  });

  it("never logs a command or a payload", async () => {
    const sock = tmpSock();
    const h = await new Home({ sshSock: sock }).up();
    const w = wrapper(sock, ["web-1", "echo SECRETCOMMAND"]);
    await opened(h);
    w.p.stdin!.end("SECRETINPUT");
    h.sendRaw({ jsonrpc: "2.0", method: "ssh.data", params: { stream: "s1", fd: 1, b64: b64("SECRETOUTPUT") } });
    h.sendRaw({ jsonrpc: "2.0", method: "ssh.exit", params: { stream: "s1", code: 0 } });
    await w.done;
    h.closeStdin();
    await tick(30);
    assert.ok(h.logs.some((m) => m.startsWith("ssh.open")));
    assert.equal(h.logs.join("\n").includes("SECRET"), false);
  });
});
