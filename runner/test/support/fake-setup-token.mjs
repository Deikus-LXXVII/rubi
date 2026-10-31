// Plays `claude setup-token` under a terminal, from a recorded shape: an Ink screen with the link in OSC 8
// hyperlink escapes (redrawn), the paste prompt, and after a code the token (a made-up one). Used by
// test/token.test.ts through RUBI_SETUP_TOKEN_CMD.
const ESC = "\x1b";
const link = process.env.FAKE_LINK ?? "https://claude.com/cai/oauth/authorize?code=true&client_id=00000000-0000-0000-0000-000000000000&response_type=code&state=abc_DEF-123";
const token = process.env.FAKE_TOKEN ?? "sk-ant-oat01-FAKEFAKE0123456789abcdefghijklmnopqrstuvwxyz_ABCDEFGHIJKLMNOPQRSTUV-0123456789";
const hyper = (u) => `${ESC}]8;;${u}${ESC}\\${u}${ESC}]8;;${ESC}\\`;
const w = (s) => process.stdout.write(s);
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

w(`${ESC}[?25l${ESC}[2J${ESC}[HClaude Code long-lived token\r\n\r\n`);
w(`Browser didn't open? Use the url below to sign in\r\n${hyper(link)}\r\n`);
await wait(20);
w(`${ESC}[2K${ESC}[1A${hyper(link)}\r\n`); // a redraw
w(`${ESC}[1mPaste code here if prompted >${ESC}[22m `);
if (process.env.FAKE_NO_CODE === "1") process.exit(0);
let got = "";
process.stdin.on("data", async (d) => {
  got += d.toString();
  if (!got.includes("\r")) return;
  if (process.env.FAKE_BAD_CODE === "1" && got.trim() !== "good") {
    got = "";
    w(`\r\n${ESC}[31mInvalid code${ESC}[0m\r\n${ESC}[1mPaste code here if prompted >${ESC}[22m `);
    return;
  }
  w(`\r\nYour OAuth token (valid for 1 year):\r\n`);
  const half = Math.floor(token.length / 2);
  w(`${ESC}[32m${token.slice(0, half)}`); // the token arrives in two pieces
  await wait(30);
  w(`${token.slice(half)}${ESC}[0m\r\n\r\nStore this token securely.\r\n`);
  await wait(30);
  process.exit(0);
});
