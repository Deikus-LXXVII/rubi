// Entry point: stdin/stdout are the protocol channel, everything else goes to stderr.
import { Runner } from "./runner.js";
import { realSdk } from "./sdk-real.js";
import { RUNNER_VERSION } from "./version.js";

console.log = console.error; // stdout belongs to the protocol
const runner = new Runner(process.stdin, process.stdout, {
  sdk: realSdk,
  version: RUNNER_VERSION,
  env: process.env,
  cwd: process.env.RUBI_WORK_DIR || "/work",
  log: (m) => process.stderr.write(`[runner] ${m}\n`),
  exit: (code) => setImmediate(() => process.exit(code)),
});
process.on("uncaughtException", (e) => {
  process.stderr.write(`[runner] uncaught: ${e instanceof Error ? e.stack : String(e)}\n`);
  process.exit(1);
});
process.on("unhandledRejection", (e) => {
  process.stderr.write(`[runner] unhandled rejection: ${e instanceof Error ? e.stack : String(e)}\n`);
});
void runner.start();
