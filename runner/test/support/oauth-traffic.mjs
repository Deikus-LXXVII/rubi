// Spike (run by hand): what does the real bundled `claude` send when it runs with a base URL and an OAuth token
// placeholder? Records every request to the base URL (header names; auth values redacted) and every connection
// it tries to a fixed host (through a refusing CONNECT proxy). Findings: docs/design/home-setup-wizard.md,
// "Shared sign-in". Usage: node runner/test/support/oauth-traffic.mjs [plain|image]  (needs npm ci in runner/)
// Records everything claude sends when it runs with a base URL and an OAuth token placeholder.
import http from "node:http";
import net from "node:net";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
const R = new URL("../..", import.meta.url).pathname.replace(/\/$/, ""); // runner/
const { startMock } = await import(R + "/test/support/mock.mjs");
const sdk = await import(R + "/node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs");

const mock = await startMock({ one: [{ text: "ONE-DONE" }] });
const rec = [];
// recorder in front of the mock
const front = http.createServer((req, res) => {
  const chunks = [];
  req.on("data", (c) => chunks.push(c));
  req.on("end", () => {
    const h = {};
    for (const [k, v] of Object.entries(req.headers)) {
      h[k] = ["authorization"].includes(k) ? String(v).split(" ")[0] + " <redacted>" : ["anthropic-beta", "anthropic-version", "content-type", "user-agent", "x-app"].includes(k) ? v : "<set>";
    }
    rec.push({ method: req.method, path: req.url.split("?")[0], query: req.url.includes("?") ? req.url.split("?")[1].replace(/=[^&]*/g, "=…") : "", headers: h, bytes: Buffer.concat(chunks).length });
    const up = http.request(mock.url + req.url, { method: req.method, headers: req.headers }, (r) => { res.writeHead(r.statusCode, r.headers); r.pipe(res); });
    up.on("error", () => res.end());
    up.end(Buffer.concat(chunks));
  });
});
await new Promise((r) => front.listen(0, "127.0.0.1", r));
const base = `http://127.0.0.1:${front.address().port}`;
// a CONNECT recorder: any https traffic claude sends elsewhere shows up here, and is refused
const connects = [];
const proxy = http.createServer((q, s) => { connects.push(`HTTP ${q.method} ${q.url}`); s.writeHead(403).end(); });
proxy.on("connect", (req, sock) => { connects.push("CONNECT " + req.url); sock.end("HTTP/1.1 403 Forbidden\r\n\r\n"); });
await new Promise((r) => proxy.listen(0, "127.0.0.1", r));
const proxyUrl = `http://127.0.0.1:${proxy.address().port}`;

const work = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "spike-work-")));
const home = fs.mkdtempSync(path.join(os.tmpdir(), "spike-home-"));
const mode = process.argv[2] || "plain";
const env = { PATH: process.env.PATH, HOME: home, CLAUDE_CONFIG_DIR: path.join(home, "cfg"), ANTHROPIC_BASE_URL: base,
  CLAUDE_CODE_OAUTH_TOKEN: process.env.PLACEHOLDER || "per-start-secret-" + "x".repeat(40), HTTPS_PROXY: proxyUrl, https_proxy: proxyUrl, NO_PROXY: "127.0.0.1", no_proxy: "127.0.0.1" };
if (mode === "image") Object.assign(env, { DISABLE_TELEMETRY: "1", DISABLE_ERROR_REPORTING: "1", CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1" });
const ac = new AbortController();
setTimeout(() => ac.abort(), 60000);
const q = sdk.query({ prompt: "MOCKSCRIPT:one say hi", options: { cwd: work, env, permissionMode: "bypassPermissions", allowDangerouslySkipPermissions: true, abortController: ac, settingSources: [], extraArgs: { "debug-file": path.join(home, "debug.log") }, stderr: (d) => (process.env.SHOWERR ? process.stderr.write(d) : 0) } });
let result = null, init = null;
try {
  for await (const m of q) {
    if (m.type === "system" && m.subtype === "init") init = { model: m.model, apiKeySource: m.apiKeySource };
    if (m.type === "result") result = { subtype: m.subtype, is_error: m.is_error, result: String(m.result).slice(0, 80) };
  }
} catch (e) { result = { thrown: String(e).slice(0, 200) }; }
let models = null;
try { const ac3 = new AbortController(); const q3 = sdk.query({ prompt: (async function* () { await new Promise((r) => ac3.signal.addEventListener("abort", r)); })(), options: { cwd: work, env, abortController: ac3, settingSources: [], persistSession: false } }); models = (await q3.supportedModels()).length; ac3.abort(); } catch (e) { models = String(e).slice(0, 100); }
// the account/usage probe the runner's usage.probe makes
let account = null, usage = null;
try {
  const ac2 = new AbortController();
  const q2 = sdk.query({ prompt: (async function* () { await new Promise((r) => ac2.signal.addEventListener("abort", r)); })(), options: { cwd: work, env, abortController: ac2, settingSources: [], persistSession: false, allowedTools: [], mcpServers: {}, strictMcpConfig: true } });
  const ir = await q2.initializationResult();
  account = ir.account;
  try { const u = await q2.usage_EXPERIMENTAL_MAY_CHANGE_DO_NOT_RELY_ON_THIS_API_YET({ skipBehaviors: true }); usage = { rate_limits_available: u.rate_limits_available }; } catch (e) { usage = { error: String(e).slice(0, 120) }; }
  ac2.abort();
} catch (e) { account = { error: String(e).slice(0, 160) }; }
const dbg = fs.existsSync(path.join(home, "debug.log")) ? fs.readFileSync(path.join(home, "debug.log"), "utf8") : ""; fs.writeFileSync(process.env.DBG || "debug-" + mode + ".log", dbg);
console.log(JSON.stringify({ mode, models, init, result, account, usage, requests: rec, connects }, null, 1));
front.close(); proxy.close(); mock.close();
process.exit(0);
