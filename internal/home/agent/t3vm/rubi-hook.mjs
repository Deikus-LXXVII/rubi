// In the VM: the PreToolUse command hook of the managed settings. It hands the hook input to Rubi Home's gate (the
// netpipe forwarder listens on 127.0.0.1:3130) and prints the answer.
//
// Nothing here is configurable from outside: a repository's .claude/settings.json `env` block reaches hook processes,
// so an address or a timeout read from the environment would let a project talk its way past the gate. The only
// inputs are stdin and the two constants below.
//
// Every failure is a block: the deny JSON on stdout AND exit status 2 (Claude Code 2.1.314, checked in the runner
// image: deny JSON with exit 0 blocks the tool, exit 2 blocks it, but exit 1, a crash, an unhandled rejection or
// output that isn't JSON let the tool RUN).
"use strict";
const GATE_URL = "http://127.0.0.1:3130/ask";
const TIMEOUT_MS = 295000;

function block(why) {
  const out = JSON.stringify({ hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: why } }) + "\n";
  try {
    process.stderr.write(why + "\n");
    process.stdout.write(out, () => process.exit(2));
  } catch {
    process.exit(2);
  }
  setTimeout(() => process.exit(2), 2000).unref();
}

process.on("uncaughtException", (e) => block("Rubi hook failed: " + (e && e.message ? e.message : e)));
process.on("unhandledRejection", (e) => block("Rubi hook failed: " + (e && e.message ? e.message : e)));
process.stdout.on("error", () => process.exit(2));

let input = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (c) => (input += c));
process.stdin.on("error", (e) => block("Rubi hook could not read its input: " + e.message));
process.stdin.on("end", async () => {
  try {
    const u = new URL(GATE_URL);
    if (u.protocol !== "http:" || u.hostname !== "127.0.0.1") return block("Rubi hook is misconfigured");
    const r = await fetch(u, { method: "POST", headers: { "content-type": "application/json" }, body: input, redirect: "error", signal: AbortSignal.timeout(TIMEOUT_MS) });
    if (!r.ok) return block(`Rubi gate answered ${r.status}`);
    const text = await r.text();
    let v;
    try {
      v = JSON.parse(text);
    } catch {
      return block("Rubi gate answered something that is not JSON");
    }
    if (v === null || typeof v !== "object" || Array.isArray(v)) return block("Rubi gate answered something that is not an object");
    process.stdout.write(JSON.stringify(v) + "\n");
  } catch (e) {
    block(`Rubi gate unreachable: ${e && e.message ? e.message : e}`);
  }
});
