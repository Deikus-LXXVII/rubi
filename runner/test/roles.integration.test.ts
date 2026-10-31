// Opt-in (RUBI_RUNNER_INTEGRATION=1): role mode against the real Agent SDK and the real bundled `claude`, with a
// scripted local API instead of the model. The isolation matrix of docs/design/agent-roles.md (section 1.1 #14 and
// friends): two roles on one repository see only their own skills, agents, tools and MCP servers, and nothing of the
// repository's `.claude`. Needs Node >= 22 (CI uses 26).
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { after, before, describe, it } from "node:test";
import { fileURLToPath } from "node:url";
import { Home, TOKEN } from "./harness.js";
import { realSdk } from "../src/sdk-real.js";
import type { Sdk, SdkMessage, SdkQuery } from "../src/sdk.js";

const enabled = process.env.RUBI_RUNNER_INTEGRATION === "1";
const here = path.dirname(fileURLToPath(import.meta.url));
type Msg = Record<string, any>;

const w = (p: string, s: string) => {
  fs.mkdirSync(path.dirname(p), { recursive: true });
  fs.writeFileSync(p, s);
};
const skill = (dir: string, name: string, extra = "") => w(path.join(dir, name, "SKILL.md"), `---\nname: ${name}\ndescription: ${name.toUpperCase()}-DESC use for ${name} things\n${extra}---\n\nBODY-OF-${name.toUpperCase()}\n`);
const agent = (dir: string, name: string, tools = "Read") => w(path.join(dir, `${name}.md`), `---\nname: ${name}\ndescription: ${name.toUpperCase()}-DESC agent\ntools: ${tools}\n---\n\nYou are ${name}.\n`);

/** The real SDK, with every `init` message kept. */
function tapSdk(inits: Msg[]): Sdk {
  return {
    ...realSdk,
    query(p) {
      const q = realSdk.query(p);
      const tapped: SdkQuery = {
        interrupt: () => q.interrupt(),
        setPermissionMode: (m) => q.setPermissionMode(m),
        close: () => q.close(),
        async *[Symbol.asyncIterator](): AsyncGenerator<SdkMessage> {
          for await (const m of q) {
            if (m.type === "system" && m.subtype === "init") inits.push(m);
            yield m;
          }
        },
      };
      return tapped;
    },
  };
}

describe("roles: real SDK + real claude against the scripted API", { skip: !enabled }, () => {
  let mock: { url: string; requests: any[]; others: string[]; close: () => void };
  let root: string, work: string, userHome: string;
  const roleDirs: Record<string, string> = {};
  const claudeDirs: Record<string, string> = {};
  const homes: Home[] = [];
  const m = (f: string) => path.join(work, f);

  /** Writes role `name` as Home would: <dir>/v1/{role.json,plugin/} and current -> v1. */
  function makeRole(name: string, role: Msg, build: (plugin: string) => void) {
    const dir = path.join(root, `role-${name}`);
    const plugin = path.join(dir, "v1/plugin");
    w(path.join(plugin, ".claude-plugin/plugin.json"), JSON.stringify({ name: "role", version: "1" }));
    build(plugin);
    w(path.join(dir, "v1/role.json"), JSON.stringify({ role: `r_${name}`, version: 1, project_instructions: true, ...role }));
    fs.symlinkSync("v1", path.join(dir, "current"));
    roleDirs[name] = dir;
    claudeDirs[name] = path.join(root, `claude-${name}`);
    fs.mkdirSync(claudeDirs[name]!, { recursive: true });
  }

  /** A runner in this process for role `name`; every init message of its queries lands in `inits`. */
  async function runner(name: string, mode = "bypassPermissions") {
    const inits: Msg[] = [];
    const h = new Home({
      sdk: tapSdk(inits) as never,
      cwd: work,
      roleDir: roleDirs[name]!,
      configDir: claudeDirs[name]!,
      env: { PATH: process.env.PATH ?? "", RUBI_RUN_TOKEN: TOKEN, HOME: userHome, ANTHROPIC_BASE_URL: mock.url, CLAUDE_CODE_OAUTH_TOKEN: "sk-ant-oat01-placeholder-per-start", HTTPS_PROXY: "", NO_PROXY: "127.0.0.1", DISABLE_TELEMETRY: "1", CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1" },
    } as never);
    homes.push(h);
    await h.up();
    const run = async (script: string, text = "go") => {
      const from = mock.requests.length;
      const { id } = await h.ok("session.start", { prompt: `MOCKSCRIPT:${script} ${text}`, mode });
      await h.waitFor((l) => l.from === "runner" && l.msg.method === "event" && l.msg.params.session_id === id && l.msg.params.kind === "result", 90_000);
      return { id, reqs: mock.requests.slice(from), init: inits.at(-1)! };
    };
    return { h, inits, run };
  }
  const results = (reqs: any[]) => JSON.stringify(reqs.flatMap((r) => r.saw_tool_results));
  const everything = (reqs: any[]) => reqs.map((r) => `${r.system}\n${r.ctx}`).join("\n");

  before(async () => {
    const { startMock } = (await import(path.join(here, "../../test/support/mock.mjs"))) as any;
    root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "roles-it-")));
    work = path.join(root, "work");
    userHome = path.join(root, "home");
    fs.mkdirSync(work);
    execFileSync("git", ["init", "-q", "-b", "main", work]);
    // The shared repository: everything the legacy mode would load.
    w(m("CLAUDE.md"), "REPO-CLAUDE-MD-SENTINEL\n");
    skill(m(".claude/skills"), "repo-skill");
    agent(m(".claude/agents"), "repo-agent");
    w(m(".claude/commands/repo-cmd.md"), "---\ndescription: REPO-CMD-DESC\n---\nrepo command\n");
    w(m(".claude/settings.json"), JSON.stringify({ env: { REPO_ENV_SENTINEL: "1" }, permissions: { allow: ["Bash(touch:*)"] } }));
    w(m(".mcp.json"), JSON.stringify({ mcpServers: { repomcp: { command: "/bin/sh", args: ["-c", `touch ${m("m-repo-mcp")}; sleep 5`] } } }));
    skill(m("sub/.claude/skills"), "nested-skill");
    w(m("sub/file.txt"), "hello\n");
    // The user's own ~/.claude, which no role may see.
    skill(path.join(userHome, ".claude/skills"), "home-skill");
    w(path.join(userHome, ".claude/CLAUDE.md"), "HOME-CLAUDE-MD-SENTINEL\n");

    makeRole("a", { skills: ["a-design"], tools: ["Read", "Edit", "Write", "Glob", "Grep", "Bash", "Agent", "Skill"], instructions: "ROLE-A-INSTRUCTIONS" }, (p) => {
      // allowed-tools is what Home strips; it is kept here on purpose, to show that it still cannot get past the gate.
      skill(path.join(p, "skills"), "a-design", "allowed-tools: Bash(touch:*)\n");
      agent(path.join(p, "agents"), "a-reviewer");
    });
    makeRole(
      "b",
      { skills: ["b-design"], tools: ["Read", "Glob", "Grep", "WebFetch", "Skill"], instructions: "ROLE-B-INSTRUCTIONS", mcp: { stdiox: { type: "stdio", command: process.execPath, args: [path.join(here, "../../test/support/mcp-stdio.mjs")] } } },
      (p) => {
        skill(path.join(p, "skills"), "b-design");
        agent(path.join(p, "agents"), "b-reviewer");
      },
    );
    // A role with no skills whose subagent definition asks for a tool the role does not have.
    makeRole("c", { skills: [], tools: ["Read", "Agent"] }, (p) => agent(path.join(p, "agents"), "widener", "Bash, Read"));

    mock = await startMock({
      plain: [{ text: "PLAIN-DONE" }],
      nested: [{ tools: [{ name: "Read", input: { file_path: m("sub/file.txt") } }] }, { tools: [{ name: "Read", input: { file_path: m("CLAUDE.md") } }] }, { text: "NESTED-DONE" }],
      skAD: [{ tools: [{ name: "Skill", input: { skill: "role:a-design" } }] }, { text: "AD-DONE" }],
      skBD: [{ tools: [{ name: "Skill", input: { skill: "role:b-design" } }] }, { text: "BD-DONE" }],
      skBDplain: [{ tools: [{ name: "Skill", input: { skill: "b-design" } }] }, { text: "BDP-DONE" }],
      skRepo: [{ tools: [{ name: "Skill", input: { skill: "repo-skill" } }] }, { text: "REPO-DONE" }],
      skHome: [{ tools: [{ name: "Skill", input: { skill: "home-skill" } }] }, { text: "HOME-DONE" }],
      skViz: [{ tools: [{ name: "Skill", input: { skill: "dataviz" } }] }, { text: "VIZ-DONE" }],
      envEcho: [{ tools: [{ name: "Bash", input: { command: "echo REPO=[$REPO_ENV_SENTINEL]", description: "e" } }] }, { text: "ENV-DONE" }],
      skillBash: [{ tools: [{ name: "Skill", input: { skill: "role:a-design" } }] }, { tools: [{ name: "Bash", input: { command: `touch ${m("m-skill-bash")}`, description: "t" } }] }, { text: "SKILLBASH-DONE" }],
      subWiden: [{ tools: [{ name: "$AGENT", input: { subagent_type: "role:widener", description: "w", prompt: "MOCKSCRIPT:subBash go" } }] }, { text: "SUBW-DONE" }],
      subBash: [{ tools: [{ name: "Bash", input: { command: `touch ${m("m-sub-bash")}`, description: "t" } }] }, { text: "SUBBASH-DONE" }],
    });
  });

  after(async () => {
    for (const h of homes) h.closeStdin();
    await new Promise((r) => setTimeout(r, 300));
    mock?.close();
    if (root) {
      try {
        execFileSync("chmod", ["-R", "u+w", root]);
      } catch {
        // best effort
      }
      fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 });
    }
  });

  it("role A and role B on one repository: skills, agents, tools and MCP servers differ, and no repo .claude shows", async () => {
    const A = await runner("a");
    const a = await A.run("plain");
    const B = await runner("b");
    const b = await B.run("plain");

    const skills = (i: Msg) => (i.skills as string[]).filter((s) => s.includes("design") || /repo|nested|home/.test(s));
    assert.deepEqual(skills(a.init), ["role:a-design"]);
    assert.deepEqual(skills(b.init), ["role:b-design"]);
    for (const i of [a.init, b.init]) {
      assert.ok(!(i.skills as string[]).some((s) => /repo-skill|nested-skill|home-skill/.test(s)), `skills: ${i.skills}`);
      assert.ok(!(i.agents as string[]).some((s) => /repo-agent/.test(s)), `agents: ${i.agents}`);
      assert.ok(!(i.slash_commands as string[]).some((s) => /repo-cmd/.test(s)), "commands");
      assert.ok(!(i.tools as string[]).some((t) => /^(Cron|Schedule|Workflow|Monitor|SendMessage|EnterWorktree|ToolSearch|NotebookEdit|WebSearch)/.test(t)), `default extras gone: ${i.tools}`);
    }
    assert.ok((a.init.agents as string[]).includes("role:a-reviewer"));
    assert.ok(!(a.init.agents as string[]).includes("role:b-reviewer"));
    assert.ok((b.init.agents as string[]).includes("role:b-reviewer"));
    assert.ok(!(b.init.agents as string[]).includes("role:a-reviewer"));

    assert.deepEqual([...a.init.tools].sort(), ["AskUserQuestion", "Bash", "Edit", "Glob", "Grep", "Read", "Skill", "Task", "Write"]);
    assert.deepEqual([...b.init.tools].sort(), ["AskUserQuestion", "Glob", "Grep", "Read", "Skill", "WebFetch", "mcp__stdiox__echo"]);
    assert.deepEqual((a.init.mcp_servers as Msg[]).map((s) => s.name), []);
    assert.deepEqual((b.init.mcp_servers as Msg[]).map((s) => s.name), ["stdiox"]);
    assert.equal(b.init.mcp_servers[0].status, "connected");

    // What the model was shown.
    const sa = everything(a.reqs), sb = everything(b.reqs);
    assert.match(sa, /A-DESIGN-DESC/);
    assert.match(sa, /A-REVIEWER-DESC/);
    assert.doesNotMatch(sa, /B-DESIGN-DESC|B-REVIEWER-DESC|STDIO-ECHO-DESC/);
    assert.match(sb, /B-DESIGN-DESC/);
    assert.doesNotMatch(sb, /A-DESIGN-DESC|A-REVIEWER-DESC/);
    for (const s of [sa, sb]) assert.doesNotMatch(s, /REPO-SKILL-DESC|REPO-AGENT-DESC|REPO-CMD-DESC|NESTED-SKILL-DESC|HOME-SKILL-DESC|HOME-CLAUDE-MD-SENTINEL|dataviz/);
    assert.match(sa, /ROLE-A-INSTRUCTIONS/);
    assert.doesNotMatch(sa, /ROLE-B-INSTRUCTIONS/);
    // The tools offered to the model match init.tools; the MCP tool's description goes to the model directly.
    assert.ok(a.reqs[0].tools.includes("Bash") && !a.reqs[0].tools.includes("WebFetch"));
    assert.ok(b.reqs[0].tools.includes("mcp__stdiox__echo") && !b.reqs[0].tools.includes("Bash"));
    await new Promise((r) => setTimeout(r, 800));
    assert.equal(fs.existsSync(m("m-repo-mcp")), false, "the repo's .mcp.json server never started");
  });

  it("the repo's CLAUDE.md reaches the model through the system prompt, labelled as project text", async () => {
    const A = await runner("a");
    const { reqs } = await A.run("plain");
    assert.match(reqs[0].system, /REPO-CLAUDE-MD-SENTINEL/);
    assert.match(reqs[0].system, /written in the repository/);
  });

  it("the Skill tool refuses a skill of another role, of the repo, of the user and a bundled one", async () => {
    const A = await runner("a");
    const B = await runner("b");
    const ok = await A.run("skAD");
    assert.doesNotMatch(results(ok.reqs), /Unknown skill|not in this session/);
    for (const [r, script] of [[A, "skBD"], [A, "skBDplain"], [A, "skRepo"], [A, "skHome"], [A, "skViz"], [B, "skAD"]] as const) {
      const { reqs } = await r.run(script);
      assert.match(results(reqs), /Unknown skill|not in this session's skills allowlist/, script);
    }
  });

  it("nothing from the repo's .claude appears after the model reads a file under sub/ (no nested discovery)", async () => {
    const A = await runner("a");
    const { reqs } = await A.run("nested");
    assert.ok(reqs.length >= 3);
    assert.doesNotMatch(reqs.map((r) => r.raw).join("\n"), /NESTED-SKILL-DESC|nested-skill/);
  });

  it("the project's settings.json env does not apply", async () => {
    const A = await runner("a");
    const { reqs } = await A.run("envEcho");
    assert.match(results(reqs), /REPO=\[\]/);
  });

  it("a skill's allowed-tools cannot get past the permission gate: Bash is denied by Home", async () => {
    const A = await runner("a", "default");
    A.h.askHandler = (p) => (p.tool === "Bash" ? { behavior: "deny", message: "denied by the gate" } : { behavior: "allow" });
    const { reqs } = await A.run("skillBash");
    assert.ok(A.h.asks.some((x) => x.tool === "Bash"), "Bash was asked about");
    assert.equal(fs.existsSync(m("m-skill-bash")), false);
    assert.match(results(reqs), /denied by the gate/);
  });

  it("a subagent definition cannot widen the role's tools", async () => {
    const C = await runner("c");
    const { reqs, init } = await C.run("subWiden");
    assert.ok((init.agents as string[]).includes("role:widener"));
    await C.h.waitFor((l) => l.msg.params?.kind === "status" && l.msg.params.status === "idle", 30_000);
    assert.equal(fs.existsSync(m("m-sub-bash")), false);
    assert.match(JSON.stringify(mock.requests.flatMap((r) => r.saw_tool_results)), /No such tool available: Bash/);
    assert.ok(reqs.length >= 1);
  });

  it("requests go to the base URL with the placeholder bearer token and no other path", async () => {
    const A = await runner("a");
    await A.run("plain");
    const mine = mock.requests.slice(-3);
    assert.ok(mine.every((r) => r.auth === "Bearer sk-ant-oat01-placeholder-per-start"), JSON.stringify(mine.map((r) => r.auth)));
    assert.deepEqual([...new Set(mock.requests.map((r) => r.path))].filter((p) => !/\/v1\/messages(\/count_tokens)?$/.test(p)), []);
    assert.deepEqual(mock.others, []);
  });

  it("sessions are listed per config dir, through the runner process (RUBI_CLAUDE_DIR)", async () => {
    // Each role's runner is a process of its own in production; here too, so CLAUDE_CONFIG_DIR is per process.
    const lists: Record<string, string[]> = {};
    for (const name of ["a", "b"]) {
      const cfgDir = path.join(root, `claude-list-${name}`); // fresh: the earlier tests' sessions live in claudeDirs
      fs.mkdirSync(cfgDir);
      const p = spawn(process.execPath, [path.join(here, "../src/start.js")], {
        cwd: work,
        stdio: "pipe",
        env: { PATH: process.env.PATH ?? "", HOME: userHome, RUBI_RUN_TOKEN: TOKEN, RUBI_WORK_DIR: work, RUBI_ROLE_DIR: roleDirs[name]!, RUBI_CLAUDE_DIR: cfgDir, ANTHROPIC_BASE_URL: mock.url, CLAUDE_CODE_OAUTH_TOKEN: "sk-ant-oat01-placeholder-per-start", NO_PROXY: "127.0.0.1", RUBI_CRED_SOCK: path.join(root, `cred-${name}.sock`) },
      });
      const lines: Msg[] = [];
      let buf = "";
      const waiters: (() => void)[] = [];
      p.stdout.on("data", (c: Buffer) => {
        buf += c.toString("utf8");
        let i;
        while ((i = buf.indexOf("\n")) >= 0) {
          const msg = JSON.parse(buf.slice(0, i)) as Msg;
          buf = buf.slice(i + 1);
          lines.push(msg);
          if (msg.method === "hello") p.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { ok: true } }) + "\n");
          if (msg.method === "perm.ask") p.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { behavior: "allow" } }) + "\n");
          waiters.splice(0).forEach((f) => f());
        }
      });
      const wait = async (pred: () => boolean) => {
        const end = Date.now() + 90_000;
        while (!pred()) {
          if (Date.now() > end) throw new Error("timeout; lines: " + JSON.stringify(lines).slice(-800));
          await new Promise<void>((r) => (waiters.push(r), setTimeout(r, 200)));
        }
      };
      p.stderr.resume();
      const call = async (id: number, method: string, params: Msg) => {
        p.stdin.write(JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n");
        await wait(() => lines.some((l) => l.id === id));
        const r = lines.find((l) => l.id === id)!;
        assert.ok(!r.error, JSON.stringify(r.error));
        return r.result as Msg;
      };
      await wait(() => lines.some((l) => l.method === "hello"));
      const { id } = await call(1001, "session.start", { prompt: `MOCKSCRIPT:plain from ${name}`, mode: "bypassPermissions" });
      await wait(() => lines.some((l) => l.method === "event" && l.params.session_id === id && l.params.kind === "result"));
      lists[name] = ((await call(1002, "session.list", {})) as { sessions: Msg[] }).sessions.map((s) => s.id);
      assert.deepEqual(lists[name], [id]);
      p.stdin.end();
      p.kill("SIGTERM");
      assert.ok(fs.readdirSync(cfgDir).includes("projects"), "the sessions are written under RUBI_CLAUDE_DIR");
    }
    assert.ok(!fs.existsSync(path.join(work, ".rubi")), "nothing is written under /work/.rubi in role mode");
    assert.notEqual(lists.a![0], lists.b![0]);
  });
});
