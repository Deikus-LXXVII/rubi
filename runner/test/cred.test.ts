import assert from "node:assert/strict";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { describe, it } from "node:test";
import { Home, tick } from "./harness.js";

const tmpSock = () => path.join(fs.mkdtempSync(path.join(os.tmpdir(), "rc-")), "cred.sock");

/** What git-credential-rubi does: op line, git's text, read the answer until the runner closes. */
function helper(sock: string, op: string, text: string): Promise<string> {
  return new Promise((resolve) => {
    const c = net.connect(sock);
    let out = "";
    c.setEncoding("utf8");
    c.on("data", (d: string) => (out += d));
    c.on("close", () => resolve(out));
    c.on("error", () => resolve(out)); // a dropped connection (ECONNRESET) = no answer
    c.write(`${op}\n${text}`);
  });
}

const GET = "protocol=https\nhost=git.example.com\npath=alice/my-project.git\n\n";

describe("git credential socket", () => {
  it("is created 0600 after the handshake and removed on exit", async () => {
    const sock = tmpSock();
    const h = await new Home({ credSock: sock }).up();
    assert.equal(fs.statSync(sock).mode & 0o777, 0o600);
    h.closeStdin();
    await tick(30);
    assert.equal(fs.existsSync(sock), false);
  });

  it("relays get as git.credential {op, input, purpose:read} and returns Home's text", async () => {
    const sock = tmpSock();
    const h = await new Home({ credSock: sock }).up();
    h.credHandler = () => ({ output: "protocol=https\nhost=git.example.com\nusername=alice\npassword=tok\n\n" });
    const out = await helper(sock, "get", GET);
    assert.equal(out, "protocol=https\nhost=git.example.com\nusername=alice\npassword=tok\n\n");
    assert.deepEqual(h.creds, [{ op: "get", input: GET, purpose: "read" }]);
    h.closeStdin();
  });

  it("purpose is push only after Home approved a git push command, for a short window", async () => {
    const sock = tmpSock();
    const h = await new Home({ credSock: sock }).up();
    h.sdk.turn = async (ctx) => {
      await ctx.tool("Bash", { command: "git -C /work push origin main" });
      ctx.result();
    };
    await helper(sock, "get", GET);
    await h.ok("session.start", { prompt: "push it", mode: "bypassPermissions" });
    await h.waitEvent("result");
    await helper(sock, "get", GET);
    assert.deepEqual(h.creds.map((c) => c.purpose), ["read", "push"]);
    (h.runner as any).pushUntil = Date.now() - 1; // the window is over
    await helper(sock, "get", GET);
    assert.equal(h.creds[2]!.purpose, "read");
    h.closeStdin();
  });

  it("a denied push does not mark pushing; neither does a command that only mentions another git verb", async () => {
    const sock = tmpSock();
    const h = await new Home({ credSock: sock }).up();
    h.askHandler = (p) => (/push/.test(p.input.command) ? { behavior: "deny" } : { behavior: "allow" });
    h.sdk.turn = async (ctx) => {
      await ctx.tool("Bash", { command: "git push" });
      await ctx.tool("Bash", { command: "git fetch origin" });
      ctx.result();
    };
    await h.ok("session.start", { prompt: "x", mode: "bypassPermissions" });
    await h.waitEvent("result");
    await helper(sock, "get", GET);
    assert.equal(h.creds[0]!.purpose, "read");
    h.closeStdin();
  });

  it("an error from Home or an unknown op closes the connection with no output", async () => {
    const sock = tmpSock();
    const h = await new Home({ credSock: sock }).up();
    h.credHandler = () => {
      throw new Error("boom");
    };
    assert.equal(await helper(sock, "get", GET), "");
    assert.equal(await helper(sock, "rm-rf", GET), "");
    assert.equal(h.creds.length, 1);
    h.closeStdin();
  });

  it("store and erase are relayed too; an oversized request is dropped", async () => {
    const sock = tmpSock();
    const h = await new Home({ credSock: sock }).up();
    await helper(sock, "store", GET + "x");
    await helper(sock, "erase", GET);
    assert.deepEqual(h.creds.map((c) => c.op), ["store", "erase"]);
    assert.equal(await helper(sock, "get", "a=" + "b".repeat(20000) + "\n\n"), "");
    assert.equal(h.creds.length, 2);
    h.closeStdin();
  });
});
