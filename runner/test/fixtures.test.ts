// Golden transcripts of the wire protocol for the Go client (M2) to replay. The runner is run
// against the fake SDK; the exchange is recorded, ids normalized, and compared with
// testdata/*.ndjson. UPDATE_FIXTURES=1 rewrites the files. Format: see ../README.md.
import assert from "node:assert/strict";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";
import { Home, tick, type Line } from "./harness.js";

const dir = path.join(path.dirname(fileURLToPath(import.meta.url)), "../../testdata");
const UUID = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/g;

function render(h: Home): string {
  const lines: Line[] = [...h.lines];
  const out = lines.map((l) => {
    if (l.action) return { from: l.from, action: l.action };
    if (l.raw !== undefined) return { from: l.from, raw: l.raw };
    return { from: l.from, msg: l.msg };
  });
  for (const code of h.exits) out.push({ from: "runner", action: `exit:${code}` } as never);
  let text = out.map((o) => JSON.stringify(o)).join("\n") + "\n";
  const ids = new Map<string, number>();
  text = text.replace(UUID, (u) => `<uuid-${ids.get(u) ?? (ids.set(u, ids.size + 1), ids.size)}>`);
  return text;
}

function check(name: string, h: Home) {
  const got = render(h);
  const file = path.join(dir, name + ".ndjson");
  if (process.env.UPDATE_FIXTURES === "1") {
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(file, got);
    return;
  }
  assert.equal(got, fs.readFileSync(file, "utf8"), `${name}.ndjson differs from what the runner does now (UPDATE_FIXTURES=1 to regenerate)`);
}

const idle = (h: Home, n = 1) => h.waitFor(() => h.statuses().filter((s) => s === "idle").length >= n && h.events("result").length >= n);

describe("wire transcripts (testdata/*.ndjson)", () => {
  it("handshake-ok", async () => {
    const h = await new Home().up();
    await h.ok("session.list");
    check("handshake-ok", h);
  });

  it("handshake-refused", async () => {
    const h = new Home({ env: { RUBI_RUN_TOKEN: "not-the-token" } });
    await h.runner.start();
    check("handshake-refused", h);
  });

  it("malformed-lines", async () => {
    const h = await new Home().up();
    h.writeBytes("this is not json\n");
    await h.waitFor((l) => l.msg.error?.code === -32700);
    h.writeBytes('{"jsonrpc":"2.0","id":50,"method":"no.such.method"}\n');
    await h.waitFor((l) => l.msg.id === 50);
    await h.ok("session.list");
    check("malformed-lines", h);
  });

  it("start-perm-allow-deny-subagent", async () => {
    const h = await new Home().up();
    h.askHandler = (p) => (p.agent_id ? { behavior: "deny", message: "subagents may not push" } : { behavior: "allow" });
    h.sdk.turn = async (ctx) => {
      ctx.say("starting");
      await ctx.tool("Bash", { command: "ls" });
      await ctx.tool("Bash", { command: "git push" }, { agentId: "agent-1", agentType: "pusher" });
      ctx.result();
    };
    await h.ok("session.start", { prompt: "do it", mode: "bypassPermissions" });
    await idle(h);
    check("start-perm-allow-deny-subagent", h);
  });

  it("queue-and-steer", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      if (ctx.text === "long job") await ctx.hold();
      ctx.result();
    };
    const { id } = await h.ok("session.start", { prompt: "long job", mode: "default" });
    await h.ok("session.send", { id, text: "queued while busy" });
    await h.ok("session.send", { id, text: "steer now", steer: true });
    await tick(20);
    h.sdk.last.release();
    await h.waitFor(() => h.events("result").length >= 3);
    await tick(30);
    check("queue-and-steer", h);
  });

  it("background-subagent", async () => {
    const h = await new Home().up();
    h.sdk.turn = async (ctx) => {
      ctx.emit({ type: "system", subtype: "background_tasks_changed", tasks: [{ task_id: "a1", subagent_type: "researcher", description: "dig", task_type: "local_agent" }] });
      ctx.result();
    };
    await h.ok("session.start", { prompt: "delegate", mode: "bypassPermissions" });
    await h.waitEvent("result");
    await tick(20);
    h.sdk.last.emit({ type: "system", subtype: "background_tasks_changed", tasks: [] });
    await h.waitFor(() => h.statuses().at(-1) === "idle");
    check("background-subagent", h);
  });

  it("git-credential", async () => {
    const sock = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "rc-")), "cred.sock");
    const h = await new Home({ credSock: sock }).up();
    h.credHandler = () => ({ output: "protocol=https\nhost=git.example.com\nusername=alice\npassword=example-token\n\n" });
    const ask = (op: string) =>
      new Promise<string>((resolve) => {
        const c = net.connect(sock);
        let out = "";
        c.on("data", (d) => (out += d));
        c.on("close", () => resolve(out));
        c.write(`${op}\nprotocol=https\nhost=git.example.com\npath=alice/my-project.git\n\n`);
      });
    await ask("get");
    h.sdk.turn = async (ctx) => {
      await ctx.tool("Bash", { command: "git push origin main" });
      ctx.result();
    };
    await h.ok("session.start", { prompt: "push", mode: "bypassPermissions" });
    await h.waitEvent("result");
    await ask("get");
    await tick(20);
    check("git-credential", h);
    h.closeStdin(); // closes the credential socket
  });

  it("ask-user-question", async () => {
    const h = await new Home().up();
    h.askHandler = (p) => ({ behavior: "allow", updated_input: { ...p.input, answers: { "Which one?": "A" } } });
    h.sdk.turn = async (ctx) => {
      await ctx.canUseTool("AskUserQuestion", { questions: [{ question: "Which one?" }] });
      ctx.result();
    };
    await h.ok("session.start", { prompt: "ask me", mode: "bypassPermissions" });
    await idle(h);
    check("ask-user-question", h);
  });
});
