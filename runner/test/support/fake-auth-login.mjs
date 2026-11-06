// Plays `claude auth login --claudeai` under a terminal, from the shape S0 (d) saw in the runner image: "Opening
// browser to sign in…", "If the browser didn't open, visit: <link>", "Paste code here if prompted >". After a good
// code it writes a made-up credential file to $CLAUDE_SECURESTORAGE_CONFIG_DIR/.credentials.json, as claude does, and
// ends. Used by test/login.test.ts, which runs a copy of the helper whose claude is a wrapper around this file.
//   FAKE_CURSOR_GAPS=1   the gaps between words are cursor moves (ESC[nG), as claude 2.1 draws them
//   FAKE_CURSOR_GAPS=2   cursor moves and real spaces together (more than one space between the words)
//   FAKE_BAD_CODE=1      any code but "good" gets "Invalid code" and the prompt again
//   FAKE_NO_CODE=1       the prompt, then the program ends (exit 0, no credential)
//   FAKE_LINGER=1        stays up after writing the credential
//   FAKE_CRED_MODE       the credential file's mode in octal (000: nobody can open it, only stat it)
//   FAKE_LINK            the link
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const ESC = "\x1b";
// The helper must start claude as `claude auth login --claudeai`, and nothing else.
if (process.argv.slice(2).join(" ") !== "auth login --claudeai") {
  process.stderr.write("unexpected arguments: " + process.argv.slice(2).join(" ") + "\n");
  process.exit(2);
}
const link = process.env.FAKE_LINK ?? "https://claude.com/cai/oauth/authorize?code=true&client_id=00000000-0000-0000-0000-000000000000&response_type=code&state=abc_DEF-123";
const w = (s) => process.stdout.write(s);
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
const gaps = process.env.FAKE_CURSOR_GAPS === "1" || process.env.FAKE_CURSOR_GAPS === "2";
const both = process.env.FAKE_CURSOR_GAPS === "2"; // moves and real spaces together: more than one space between the words
const prompt = () => (gaps ? `${ESC}[2GPaste${both ? "  " : ""}${ESC}[8Gcode${both ? "  " : ""}${ESC}[13Ghere${ESC}[18Gif${ESC}[21Gprompted${ESC}[30G>${ESC}[32G` : `Paste code here if prompted > `);
const visit = () => (gaps ? `${ESC}[2GIf${ESC}[5Gthe${ESC}[9Gbrowser${ESC}[17Gdidn't${ESC}[24Gopen,${ESC}[30Gvisit:${ESC}[37G${link}\r\n` : `If the browser didn't open, visit: ${link}\r\n`);

w(`Opening browser to sign in…\r\n`);
await wait(20);
w(visit());
w(`${ESC}[2K${ESC}[1A${visit()}`); // a redraw
w(prompt());
if (process.env.FAKE_NO_CODE === "1") process.exit(0);
if (process.stdin.isTTY) process.stdin.setRawMode(true); // like claude's own UI: no echo, Enter arrives as \r
process.stdin.on("end", () => process.exit(0)); // the helper went away
let got = "";
process.stdin.on("data", async (d) => {
  got += d.toString();
  if (!/[\r\n]/.test(got)) return;
  if (process.env.FAKE_BAD_CODE === "1" && got.trim() !== "good") {
    got = "";
    w(`\r\n${ESC}[31mInvalid code${ESC}[0m\r\n${prompt()}`);
    return;
  }
  const dir = process.env.CLAUDE_SECURESTORAGE_CONFIG_DIR;
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, ".credentials.json"), JSON.stringify({ claudeAiOauth: { accessToken: "sk-ant-oat01-FAKEFAKE0123456789abcdefghijklmnopqrstuvwxyz", refreshToken: "sk-ant-ort01-FAKEFAKE0123456789", expiresAt: Date.now() + 3600e3 } }), { mode: parseInt(process.env.FAKE_CRED_MODE ?? "600", 8) });
  w(`\r\nLogin successful.\r\n`);
  if (process.env.FAKE_LINGER === "1") await wait(60000);
  process.exit(0);
});
