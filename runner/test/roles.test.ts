// Role mode with the fake SDK: role.json validation, the option builder, legacy mode unchanged.
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, describe, it } from "node:test";
import { Home } from "./harness.js";
import { loadRole, instructionsBlock, parseRole, ROLE_JSON_MAX } from "../src/role.js";

const tmps: string[] = [];
afterEach(() => {
  for (const d of tmps.splice(0)) fs.rmSync(d, { recursive: true, force: true });
});
const tmp = () => {
  const d = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "roles-unit-")));
  tmps.push(d);
  return d;
};

const GOOD = { role: "r_1", version: 3, skills: ["design", "notes"], tools: ["Read", "Edit", "Bash", "Agent", "Skill"], deny: ["Bash(git push:*)"], mcp: { docs: { type: "http", url: "https://mcp.example.com/" } }, instructions: "You are the designer.", project_instructions: false, default_mode: "acceptEdits", future_key: 1 };

/** <dir>/v<n>/{role.json,plugin/} and current -> v<n>; returns the role dir. */
function makeRole(role: unknown = GOOD, ver = "v3"): string {
  const dir = tmp();
  fs.mkdirSync(path.join(dir, ver, "plugin"), { recursive: true });
  fs.writeFileSync(path.join(dir, ver, "role.json"), typeof role === "string" ? role : JSON.stringify(role));
  fs.symlinkSync(ver, path.join(dir, "current"));
  return dir;
}

const homeFor = (roleDir: string, extra: Record<string, unknown> = {}) => new Home({ roleDir, configDir: "/rubi-claude", cwd: tmp(), ...extra } as never).up();

describe("role.json", () => {
  it("is parsed; unknown keys are ignored", () => {
    const r = loadRole(makeRole());
    assert.equal(r.role, "r_1");
    assert.equal(r.version, 3);
    assert.deepEqual(r.skills, ["design", "notes"]);
    assert.equal(r.default_mode, "acceptEdits");
    assert.deepEqual(r.mcp, { docs: { type: "http", url: "https://mcp.example.com/" } });
    assert.ok(r.dir.endsWith("/v3"));
  });

  it("defaults: no skills, no deny, no mcp, mode default", () => {
    const r = loadRole(makeRole({ role: "r", version: 1, tools: ["Read"] }));
    assert.deepEqual([r.skills, r.deny, r.mcp, r.default_mode, r.project_instructions], [[], [], {}, "default", false]);
  });

  for (const [what, bad] of [
    ["an unknown tool", { ...GOOD, tools: ["Read", "Teleport"] }],
    ["a tool that is not a string", { ...GOOD, tools: ["Read", 1] }],
    ["no tools", { role: "r", version: 1 }],
    ["a bad skill name", { ...GOOD, skills: ["../x"] }],
    ["a bad version", { ...GOOD, version: 0 }],
    ["no role id", { ...GOOD, role: "" }],
    ["a bad mode", { ...GOOD, default_mode: "yolo" }],
    ["instructions over 8 KiB", { ...GOOD, instructions: "x".repeat(8 * 1024 + 1) }],
    ["an mcp server of an unknown type", { ...GOOD, mcp: { x: { type: "pipe" } } }],
    ["an http mcp server without a url", { ...GOOD, mcp: { x: { type: "http" } } }],
    ["a deny rule with a control character", { ...GOOD, deny: ["Bash\n"] }],
    ["an array instead of an object", []],
  ] as const) {
    it(`refuses ${what}`, () => assert.throws(() => loadRole(makeRole(bad)), /role/i));
  }

  it("refuses invalid JSON and a file over 64 KiB", () => {
    assert.throws(() => loadRole(makeRole("{nope")), /not valid JSON/);
    assert.throws(() => loadRole(makeRole(JSON.stringify({ ...GOOD, pad: "x".repeat(ROLE_JSON_MAX) }))), /larger than/);
  });

  it("refuses a missing role.json, a missing current and a missing plugin dir", () => {
    const d = makeRole();
    fs.rmSync(path.join(d, "v3/role.json"));
    assert.throws(() => loadRole(d), /missing/);
    assert.throws(() => loadRole(tmp()), /unusable/);
    const e = makeRole();
    fs.rmSync(path.join(e, "v3/plugin"), { recursive: true });
    assert.throws(() => loadRole(e), /plugin/);
  });

  it("refuses a symlinked role.json", () => {
    const d = makeRole();
    fs.renameSync(path.join(d, "v3/role.json"), path.join(d, "real.json"));
    fs.symlinkSync("../real.json", path.join(d, "v3/role.json"));
    assert.throws(() => loadRole(d), /not a regular file/);
  });

  it("refuses a symlinked plugin dir", () => {
    const d = makeRole();
    fs.renameSync(path.join(d, "v3/plugin"), path.join(d, "elsewhere"));
    fs.symlinkSync("../elsewhere", path.join(d, "v3/plugin"));
    assert.throws(() => loadRole(d), /plain directory/);
  });

  it("refuses a current that escapes the role dir (../), absolute or relative", () => {
    const outside = tmp();
    fs.mkdirSync(path.join(outside, "plugin"));
    fs.writeFileSync(path.join(outside, "role.json"), JSON.stringify(GOOD));
    const d = tmp();
    fs.symlinkSync(outside, path.join(d, "current"));
    assert.throws(() => loadRole(d), /outside/);
    const nested = path.join(d, "inner");
    fs.mkdirSync(nested);
    fs.symlinkSync(path.relative(nested, outside), path.join(nested, "current"));
    assert.throws(() => loadRole(nested), /outside/);
  });

  it("refuses current pointing at the role dir itself", () => {
    const d = tmp();
    fs.symlinkSync(".", path.join(d, "current"));
    assert.throws(() => loadRole(d), /outside/);
  });

  it("follows current when Home swaps it to a new version", () => {
    const d = makeRole();
    fs.mkdirSync(path.join(d, "v4/plugin"), { recursive: true });
    fs.writeFileSync(path.join(d, "v4/role.json"), JSON.stringify({ ...GOOD, version: 4 }));
    fs.symlinkSync("v4", path.join(d, "current.new"));
    fs.renameSync(path.join(d, "current.new"), path.join(d, "current"));
    assert.equal(loadRole(d).version, 4);
  });

  it("parseRole is pure", () => assert.equal(parseRole(GOOD, "/x/v3").dir, "/x/v3"));
});

describe("instructionsBlock", () => {
  const role = (over: object) => parseRole({ ...GOOD, ...over }, "/x");
  it("is the role's instructions alone when project_instructions is off", () => {
    const cwd = tmp();
    fs.writeFileSync(path.join(cwd, "CLAUDE.md"), "REPO-TEXT");
    assert.equal(instructionsBlock(role({}), cwd), "You are the designer.");
  });
  it("adds CLAUDE.md and .claude/CLAUDE.md, labelled as project text", () => {
    const cwd = tmp();
    fs.mkdirSync(path.join(cwd, ".claude"));
    fs.writeFileSync(path.join(cwd, "CLAUDE.md"), "REPO-TEXT");
    fs.writeFileSync(path.join(cwd, ".claude/CLAUDE.md"), "DOT-TEXT");
    const b = instructionsBlock(role({ project_instructions: true }), cwd);
    assert.match(b, /^You are the designer\.\n\n# Project instructions/);
    assert.match(b, /written in the repository/);
    assert.ok(b.indexOf("REPO-TEXT") < b.indexOf("DOT-TEXT"));
  });
  it("skips a symlinked CLAUDE.md and respects the 64 KiB cap", () => {
    const cwd = tmp();
    fs.writeFileSync(path.join(cwd, "secret.txt"), "SECRET");
    fs.symlinkSync("secret.txt", path.join(cwd, "CLAUDE.md"));
    assert.doesNotMatch(instructionsBlock(role({ project_instructions: true }), cwd), /SECRET/);
    fs.rmSync(path.join(cwd, "CLAUDE.md"));
    fs.writeFileSync(path.join(cwd, "CLAUDE.md"), "x".repeat(64 * 1024 + 1));
    assert.doesNotMatch(instructionsBlock(role({ project_instructions: true }), cwd), /xxxx/);
    fs.writeFileSync(path.join(cwd, "CLAUDE.md"), "y".repeat(64 * 1024));
    fs.mkdirSync(path.join(cwd, ".claude"));
    fs.writeFileSync(path.join(cwd, ".claude/CLAUDE.md"), "MORE");
    assert.doesNotMatch(instructionsBlock(role({ project_instructions: true }), cwd), /MORE/);
  });
});

describe("role mode in the runner", () => {
  it("builds the query options of section 4.3", async () => {
    const h = await homeFor(makeRole());
    const { id } = await h.ok("session.start", { prompt: "go", mode: "bypassPermissions" });
    const o = h.sdk.last.options;
    assert.ok(id);
    assert.deepEqual(o.settingSources, []);
    assert.equal(o.plugins?.length, 1);
    assert.equal(o.plugins?.[0]?.type, "local");
    assert.ok(o.plugins?.[0]?.path.endsWith("/v3/plugin"));
    assert.deepEqual(o.skills, ["role:design", "role:notes"]);
    assert.deepEqual([...(o.tools ?? [])].sort(), ["Agent", "AskUserQuestion", "Bash", "Edit", "Read", "Skill"]);
    assert.equal(o.strictMcpConfig, true);
    assert.deepEqual(o.mcpServers, { docs: { type: "http", url: "https://mcp.example.com/" } });
    assert.equal(o.systemPrompt?.preset, "claude_code");
    assert.equal(o.systemPrompt?.append, "You are the designer.");
    assert.ok(o.disallowedTools.includes("Bash(git push:*)"));
    assert.ok(o.disallowedTools.includes("Edit(//rubi-claude/**)"));
    assert.ok(o.disallowedTools.includes("Read(//rubi-claude/**)"));
    assert.ok(o.disallowedTools.some((r) => r.startsWith("Edit(//") && r.endsWith("/.rubi/**)")), "the usual deny rules stay");
    assert.deepEqual(o.settings, { disableAllHooks: true, apiKeyHelper: "", skipWebFetchPreflight: true });
    assert.equal(o.env.CLAUDE_CONFIG_DIR, "/rubi-claude");
    assert.equal(o.hooks.PreToolUse.length, 2, "the permission gate and the subagent gate stay");
    assert.equal(typeof o.canUseTool, "function");
  });

  it("Skill is in the tools only when the role has skills; AskUserQuestion always; plan tools in plan mode", async () => {
    const h = await homeFor(makeRole({ ...GOOD, skills: [], tools: ["Read", "Skill", "ExitPlanMode"] }));
    await h.ok("session.start", { prompt: "go", mode: "default" });
    assert.deepEqual([...(h.sdk.last.options.tools ?? [])].sort(), ["AskUserQuestion", "Read"]);
    assert.deepEqual(h.sdk.last.options.skills, []);
    await h.ok("session.start", { prompt: "go", mode: "plan" });
    assert.deepEqual([...(h.sdk.last.options.tools ?? [])].sort(), ["AskUserQuestion", "EnterPlanMode", "ExitPlanMode", "Read"]);
  });

  it("the session mode from Home wins; default_mode is used when Home passes none", async () => {
    const h = await homeFor(makeRole());
    await h.ok("session.start", { prompt: "go", mode: "plan" });
    assert.equal(h.sdk.last.options.permissionMode, "plan");
    await h.ok("session.start", { prompt: "go" });
    assert.equal(h.sdk.last.options.permissionMode, "acceptEdits");
    const bad = await h.call("session.start", { prompt: "go", mode: "nonsense" });
    assert.ok(bad.error);
  });

  it("project instructions come from the work dir", async () => {
    const cwd = tmp();
    fs.writeFileSync(path.join(cwd, "CLAUDE.md"), "REPO-SENTINEL");
    const h = await homeFor(makeRole({ ...GOOD, project_instructions: true }), { cwd });
    await h.ok("session.start", { prompt: "go", mode: "default" });
    assert.match(h.sdk.last.options.systemPrompt?.append ?? "", /REPO-SENTINEL/);
  });

  it("reads current at each query start and pins that version for the query", async () => {
    const d = makeRole();
    const h = await homeFor(d);
    await h.ok("session.start", { prompt: "first", mode: "default" });
    assert.ok(h.sdk.last.options.plugins?.[0]?.path.includes("/v3/"));
    fs.mkdirSync(path.join(d, "v4/plugin"), { recursive: true });
    fs.writeFileSync(path.join(d, "v4/role.json"), JSON.stringify({ ...GOOD, version: 4, skills: [] }));
    fs.symlinkSync("v4", path.join(d, "n"));
    fs.renameSync(path.join(d, "n"), path.join(d, "current"));
    assert.ok(h.sdk.last.options.plugins?.[0]?.path.includes("/v3/"), "the running query keeps its version");
    await h.ok("session.start", { prompt: "second", mode: "default" });
    assert.ok(h.sdk.last.options.plugins?.[0]?.path.includes("/v4/"));
    assert.deepEqual(h.sdk.last.options.skills, []);
  });

  it("a missing or bad role.json refuses session.start; it never falls back to legacy", async () => {
    const d = makeRole("{bad");
    const h = await homeFor(d);
    const r = await h.call("session.start", { prompt: "go", mode: "default" });
    assert.ok(r.error);
    assert.match(r.error.message, /role/);
    assert.equal(h.sdk.queries.length, 0);
    assert.equal((await h.ok("session.list")).sessions.length, 0, "no half-registered session");
    fs.rmSync(path.join(d, "current"));
    assert.ok((await h.call("session.start", { prompt: "go", mode: "default" })).error);
    assert.equal(h.sdk.queries.length, 0);
  });

  it("a send to an unknown session with a bad role is refused too", async () => {
    const h = await homeFor(makeRole("{bad"));
    const r = await h.call("session.send", { id: "11111111-1111-4111-8111-111111111111", text: "hi" });
    assert.ok(r.error);
    assert.equal(h.sdk.queries.length, 0);
  });

  it("asks Home about every tool call as in legacy mode", async () => {
    const h = await homeFor(makeRole());
    h.sdk.turn = async (ctx) => {
      await ctx.tool("Bash", { command: "ls" });
      ctx.result();
    };
    await h.ok("session.start", { prompt: "go", mode: "bypassPermissions" });
    await h.waitEvent("result");
    assert.equal(h.asks[0]?.tool, "Bash");
  });
});

describe("legacy mode (no roleDir)", () => {
  it("is unchanged: project settings, no plugins, skills, tools or system prompt, mode required", async () => {
    const h = await new Home().up();
    await h.ok("session.start", { prompt: "go", mode: "default" });
    const o = h.sdk.last.options;
    assert.deepEqual(o.settingSources, ["project"]);
    for (const k of ["plugins", "skills", "tools", "systemPrompt"] as const) assert.equal(o[k], undefined, k);
    assert.deepEqual(o.mcpServers, {});
    assert.equal(o.env.CLAUDE_CONFIG_DIR, "/work/.rubi/claude");
    assert.equal(o.disallowedTools.length, 4);
    assert.ok((await h.call("session.start", { prompt: "go" })).error, "mode is still required");
  });
});
