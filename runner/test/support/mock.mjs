// Scripted stand-in for the Anthropic Messages API (no model, no cost), adapted from the M0 spike.
// The real bundled `claude` binary runs unchanged; only its model replies are scripted.
// Script selection: the LAST user text message containing "MOCKSCRIPT:<name>" picks
// scripts[name]; the step is the number of assistant turns after it.
// A step is { tools: [{name, input}] } or { text }. "$AGENT" resolves to Agent or Task.
import http from "node:http";
import fs from "node:fs";
import path from "node:path";
const require_fs = () => fs;

let n = 0;
const id = (p) => `${p}_mk${Date.now().toString(36)}${(n++).toString(36)}`;

function textOf(content) {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content.filter((b) => b.type === "text").map((b) => b.text).join("\n");
}

function pick(messages, scripts) {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (m.role !== "user") continue;
    const mm = [...textOf(m.content).matchAll(/MOCKSCRIPT:([\w-]+)/g)];
    if (mm.length) {
      const name = mm[mm.length - 1][1];
      const step = messages.slice(i + 1).filter((x) => x.role === "assistant").length;
      return { name, step, steps: scripts[name] };
    }
  }
  return { name: null, step: 0, steps: null };
}

let dumpSeq = 0;

export function startMock(scripts, log = () => {}) {
  const requests = [];
  const others = [];
  const server = http.createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      const url = req.url.split("?")[0];
      // Optional: keep every request body (and its anthropic-beta header) for checking against the gateway policy.
      if (process.env.RUBI_DUMP_API_BODIES && req.method === "POST") {
        fs.mkdirSync(process.env.RUBI_DUMP_API_BODIES, { recursive: true });
        const n = String(dumpSeq++).padStart(5, "0") + "-" + process.pid;
        fs.writeFileSync(path.join(process.env.RUBI_DUMP_API_BODIES, n + ".json"), body);
        fs.writeFileSync(path.join(process.env.RUBI_DUMP_API_BODIES, n + ".beta"), String(req.headers["anthropic-beta"] ?? ""));
      }
      if (req.method === "POST" && url.endsWith("/v1/messages/count_tokens")) {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ input_tokens: 42 }));
        return;
      }
      if (!(req.method === "POST" && url.endsWith("/v1/messages"))) {
        log("MOCK other request", req.method, url);
        others.push(`${req.method} ${url}`);
        res.writeHead(404, { "content-type": "application/json" });
        res.end(JSON.stringify({ type: "error", error: { type: "not_found_error", message: "mock" } }));
        return;
      }
      let j = {};
      try { j = JSON.parse(body); } catch {}
      const msgs = j.messages || [];
      const toolNames = (j.tools || []).map((t) => t.name);
      let { name, step, steps } = pick(msgs, scripts);
      // tool_results the model would see in this request (evidence of deny text)
      let la = -1;
      msgs.forEach((m, i) => { if (m.role === "assistant") la = i; });
      const results = msgs.slice(la + 1).flatMap((m) => Array.isArray(m.content) ? m.content : [])
        .filter((b) => b.type === "tool_result")
        .map((b) => ({ is_error: !!b.is_error, text: (typeof b.content === "string" ? b.content : textOf(b.content)).slice(0, 220) }));
      // Session-title side request (no tools, "naming a coding session"): answer with a title.
      const sysTxt = Array.isArray(j.system) ? j.system.map((b) => b.text || "").join(" ") : String(j.system || "");
      if (toolNames.length === 0 && /naming a coding session/.test(sysTxt)) { steps = null; name = `title-gen(${name})`; }
      let plan = steps && steps[step] ? steps[step] : { text: name?.startsWith("title-gen") ? "mock session" : name ? `done (${name})` : "ok" };
      const sys0 = Array.isArray(j.system) ? j.system.map((b) => b.text || "").join("\n") : String(j.system || "");
      const blocks = [];
      if (plan.text) blocks.push({ type: "text", text: plan.text });
      for (const t of plan.tools || []) {
        const tn = t.name === "$AGENT" ? (toolNames.includes("Agent") ? "Agent" : "Task") : t.name;
        blocks.push({ type: "tool_use", id: id("toolu"), name: tn, input: t.input });
      }
      const stop = blocks.some((b) => b.type === "tool_use") ? "tool_use" : "end_turn";
      const usage = { input_tokens: 100, output_tokens: 20, cache_creation_input_tokens: 0, cache_read_input_tokens: 0 };
      requests.push({ raw: body, path: url, auth: req.headers.authorization, tools: toolNames, system: sys0, ctx: msgs.map((m) => textOf(m.content)).join("\n"), model: j.model, script: name, step, stream: !!j.stream, tools_offered: toolNames.length, reply: blocks.map((b) => b.type === "tool_use" ? `tool_use:${b.name}` : "text"), saw_tool_results: results });
      if (process.env.MOCK_DUMP && toolNames.length === 0) require_fs().appendFileSync(process.env.MOCK_DUMP, JSON.stringify({ system: j.system, last: msgs[msgs.length - 1], nmsgs: msgs.length, keys: Object.keys(j) }).slice(0, 6000) + "\n");
      const sys = Array.isArray(j.system) ? j.system.map((b) => b.text || "").join(" ") : String(j.system || "");
      log("MOCK /v1/messages", { model: j.model, max_tokens: j.max_tokens, tools: toolNames.length, system_head: sys.slice(0, 70), script: name, step, reply: blocks.map((b) => b.type === "tool_use" ? `tool_use:${b.name}` : `text:${b.text.slice(0, 40)}`), saw_tool_results: results });
      const msgId = id("msg");
      if (!j.stream) {
        res.writeHead(200, { "content-type": "application/json", "request-id": id("req") });
        res.end(JSON.stringify({ id: msgId, type: "message", role: "assistant", model: j.model, content: blocks, stop_reason: stop, stop_sequence: null, usage }));
        return;
      }
      res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache", "request-id": id("req") });
      const ev = (type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`);
      ev("message_start", { message: { id: msgId, type: "message", role: "assistant", model: j.model, content: [], stop_reason: null, stop_sequence: null, usage: { ...usage, output_tokens: 1 } } });
      blocks.forEach((b, i) => {
        if (b.type === "text") {
          ev("content_block_start", { index: i, content_block: { type: "text", text: "" } });
          ev("content_block_delta", { index: i, delta: { type: "text_delta", text: b.text } });
        } else {
          ev("content_block_start", { index: i, content_block: { type: "tool_use", id: b.id, name: b.name, input: {} } });
          ev("content_block_delta", { index: i, delta: { type: "input_json_delta", partial_json: JSON.stringify(b.input) } });
        }
        ev("content_block_stop", { index: i });
      });
      ev("message_delta", { delta: { stop_reason: stop, stop_sequence: null }, usage: { output_tokens: 20 } });
      ev("message_stop", {});
      res.end();
    });
  });
  return new Promise((resolve) => server.listen(0, "127.0.0.1", () => {
    resolve({ url: `http://127.0.0.1:${server.address().port}`, requests, others, close: () => server.close() });
  }));
}
