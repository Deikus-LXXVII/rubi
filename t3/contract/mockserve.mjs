// Starts the scripted Messages API stand-in, writes its URL to the file named by argv[2], and keeps running.
import fs from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const here = path.dirname(fileURLToPath(import.meta.url));
const { startMock } = await import(path.join(here, "../../runner/test/support/mock.mjs"));
const { url } = await startMock({ hello: [{ text: "MOCK-OK hello" }] });
fs.writeFileSync(process.argv[2], url);
setInterval(() => {}, 1 << 30);
