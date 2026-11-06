// Entry point. Checks the environment first, with a readable message, then loads the runner.
const major = Number(process.versions.node.split(".")[0]);
if (!(major >= 22)) {
  process.stderr.write(`[runner] Node.js 22 or newer is required (this is ${process.versions.node}).\n`);
  process.exit(1);
}
try {
  await import("./start.js");
} catch (e) {
  process.stderr.write(`[runner] could not load the Claude Agent SDK (run npm ci in the runner directory?): ${e instanceof Error ? e.message : String(e)}\n`);
  process.exit(1);
}
