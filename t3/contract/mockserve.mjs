// Starts the scripted Messages API stand-in, writes its URL to the file named by argv[2], and keeps running.
// With argv[3] it also appends one JSON line per request to that file (what the model saw: tool results, the script
// and its step). Scripts: hello (one text reply) and bashrun (a Bash call that writes ran.txt in the cwd, then a text).
import fs from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const here = path.dirname(fileURLToPath(import.meta.url));
const { startMock } = await import(path.join(here, "../../runner/test/support/mock.mjs"));
const log = process.argv[3] ? (...a) => fs.appendFileSync(process.argv[3], JSON.stringify(a) + "\n") : undefined;
const { url } = await startMock({
  hello: [{ text: "MOCK-OK hello" }],
  bashrun: [{ tools: [{ name: "Bash", input: { command: "echo RAN > ran.txt", description: "write ran.txt" } }] }, { text: "bash step done" }],
}, log);
fs.writeFileSync(process.argv[2], url);
setInterval(() => {}, 1 << 30);
