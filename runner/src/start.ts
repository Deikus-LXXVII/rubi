// Entry point: stdin/stdout are the protocol channel, everything else goes to stderr.
import { Runner } from "./runner.js";
import { realSdk } from "./sdk-real.js";
import { RUNNER_VERSION } from "./version.js";

console.log = console.error; // stdout belongs to the protocol
const cwd = process.env.RUBI_WORK_DIR || "/work";
const configDir = `${cwd}/.rubi/claude`;
// listSessions/forkSession/... run in this process and read sessions from the config dir of THIS
// process's environment; the claude children get the same dir through their explicit env.
process.env.CLAUDE_CONFIG_DIR = configDir;
const runner = new Runner(process.stdin, process.stdout, {
  sdk: realSdk,
  version: RUNNER_VERSION,
  env: process.env,
  cwd,
  configDir,
  credSock: process.env.RUBI_CRED_SOCK || "/tmp/rubi-cred.sock",
  settingSources: process.env.RUBI_SETTING_SOURCES === "none" ? [] : ["project"],
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
