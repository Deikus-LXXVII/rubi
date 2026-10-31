// container/rubi-claude-token against a recorded-shape fake of `claude setup-token` (no terminal, no network,
// a made-up token).
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const helper = fileURLToPath(new URL("../../container/rubi-claude-token", import.meta.url));
const fake = fileURLToPath(new URL("../../test/support/fake-setup-token.mjs", import.meta.url));
const FAKE_TOKEN = "sk-ant-oat01-FAKEFAKE0123456789abcdefghijklmnopqrstuvwxyz_ABCDEFGHIJKLMNOPQRSTUV-0123456789";

interface Run {
  lines: string[];
  stdout: string;
  code: number | null;
  authDir: string;
  write(s: string): void;
  waitLine(pred: (l: string) => boolean): Promise<string>;
  done: Promise<void>;
}

function run(env: Record<string, string> = {}): Run {
  const authDir = mkdtempSync(join(tmpdir(), "rubi-auth-"));
  const p = spawn(process.execPath, [helper], {
    env: { PATH: process.env.PATH ?? "", RUBI_AUTH_DIR: authDir, RUBI_SETUP_TOKEN_CMD: `${process.execPath} ${fake}`, ...env },
    stdio: ["pipe", "pipe", "pipe"],
  });
  const r: Run = {
    lines: [],
    stdout: "",
    code: null,
    authDir,
    write: (s) => p.stdin.write(s),
    waitLine: (pred) =>
      new Promise((resolve, reject) => {
        const t = setTimeout(() => reject(new Error("no such line; saw " + JSON.stringify(r.lines))), 5000);
        const check = () => {
          const l = r.lines.find(pred);
          if (l !== undefined) {
            clearTimeout(t);
            resolve(l);
            return true;
          }
          return false;
        };
        if (check()) return;
        const iv = setInterval(() => check() && clearInterval(iv), 10);
        t.unref();
      }),
    done: new Promise((resolve) => p.on("close", (c) => ((r.code = c), resolve()))),
  };
  let buf = "";
  p.stdout.on("data", (d: Buffer) => {
    r.stdout += d.toString();
    buf += d.toString();
    let i;
    while ((i = buf.indexOf("\n")) >= 0) {
      r.lines.push(buf.slice(0, i));
      buf = buf.slice(i + 1);
    }
  });
  p.stderr.on("data", (d: Buffer) => (r.stdout += "[stderr]" + d.toString()));
  return r;
}

const clean = (r: Run) => rmSync(r.authDir, { recursive: true, force: true });

describe("rubi-claude-token", () => {
  it("relays the link and the prompt, takes the code, saves the token and tells only the protocol", async () => {
    const r = run();
    try {
      const url = await r.waitLine((l) => l.startsWith("URL "));
      assert.match(url, /^URL https:\/\/claude\.com\/cai\/oauth\/authorize\?code=true&client_id=/);
      assert.equal(url.split("\n").length, 1);
      await r.waitLine((l) => l === "CODE?");
      assert.equal(r.lines.filter((l) => l.startsWith("URL ")).length, 1, "the redraw sent the link again");
      assert.equal(r.lines.filter((l) => l === "CODE?").length, 1);
      r.write("the-code-from-the-browser\n");
      await r.done;
      assert.equal(r.code, 0, r.stdout);
      assert.deepEqual(r.lines.filter((l) => l !== "" && !l.startsWith("URL ")), ["CODE?", "SAVED"]);
      // the token is on disk, private, and nowhere in what the helper printed
      const dir = join(r.authDir, "token");
      assert.equal(readFileSync(join(dir, "oauth-token"), "utf8"), FAKE_TOKEN + "\n");
      assert.equal(statSync(join(dir, "oauth-token")).mode & 0o777, 0o600);
      assert.equal(r.stdout.includes("FAKEFAKE"), false);
      assert.equal(r.stdout.includes("sk-ant"), false);
      const meta = JSON.parse(readFileSync(join(dir, "oauth-token.meta"), "utf8"));
      assert.equal(meta.method, "setup-token");
      const days = (Date.parse(meta.expires) - Date.parse(meta.created)) / 86400e3;
      assert.equal(Math.round(days), 365);
      assert.equal(JSON.stringify(meta).includes("sk-ant"), false);
      assert.deepEqual(readdirSync(dir).sort(), ["oauth-token", "oauth-token.meta"]); // no temp files left
    } finally {
      clean(r);
    }
  });

  it("a wrong code gets the prompt again, then a good one works", async () => {
    const r = run({ FAKE_BAD_CODE: "1" });
    try {
      await r.waitLine((l) => l === "CODE?");
      r.write("nope\n");
      await new Promise((res) => setTimeout(res, 300));
      assert.equal(r.lines.filter((l) => l === "CODE?").length, 2, JSON.stringify(r.lines));
      r.write("good\n");
      await r.done;
      assert.equal(r.lines.at(-1), "SAVED");
    } finally {
      clean(r);
    }
  });

  it("refuses a link to another site and saves nothing", async () => {
    const r = run({ FAKE_LINK: "https://claude.com.evil.example.com/oauth" });
    try {
      await r.done;
      assert.equal(r.code, 1);
      assert.deepEqual(r.lines, ["FAILED unexpected sign-in link"]);
      assert.equal(existsSync(join(r.authDir, "token", "oauth-token")), false);
    } finally {
      clean(r);
    }
  });

  it("claude ending without a token is a failure", async () => {
    const r = run({ FAKE_NO_CODE: "1" });
    try {
      await r.done;
      assert.equal(r.code, 1);
      assert.equal(r.lines.at(-1), "FAILED the sign-in ended without finishing");
      assert.equal(existsSync(join(r.authDir, "token", "oauth-token")), false);
    } finally {
      clean(r);
    }
  });

  it("a link the output filter would drop fails instead of hanging, and nothing of it is printed", async () => {
    const r = run({ FAKE_LINK: "https://claude.com/x?t=sk-ant-oat01-LEAKLEAKLEAKLEAKLEAKLEAKLEAKLEAKLEAK" });
    try {
      await r.done;
      assert.equal(r.code, 1);
      assert.deepEqual(r.lines, ["FAILED unexpected sign-in link"]);
      assert.equal(r.stdout.includes("LEAK"), false);
      assert.equal(existsSync(join(r.authDir, "token", "oauth-token")), false);
    } finally {
      clean(r);
    }
  });

  it("input that isn't a visible-ASCII code never reaches claude", async () => {
    const r = run({ FAKE_BAD_CODE: "1" });
    try {
      await r.waitLine((l) => l === "CODE?");
      r.write("bad code with spaces\n");
      r.write("tab\there\n");
      r.write("caf\u00e9\n");
      await new Promise((res) => setTimeout(res, 300));
      assert.equal(r.lines.filter((l) => l === "CODE?").length, 1, "the prompt was answered: " + JSON.stringify(r.lines));
      r.write("good\n");
      await r.done;
      assert.equal(r.lines.at(-1), "SAVED");
    } finally {
      clean(r);
    }
  });

  it("a failed save leaves no temporary file, and no live token is reported as failed", async () => {
    const r = run();
    try {
      await r.waitLine((l) => l === "CODE?");
      // make the token's rename fail: a directory is where the token file goes
      mkdirSync(join(r.authDir, "token", "oauth-token"), { recursive: true });
      r.write("code\n");
      await r.done;
      assert.equal(r.code, 1);
      assert.equal(r.lines.at(-1), "FAILED could not save the credential");
      const left = readdirSync(join(r.authDir, "token")).filter((f) => f.endsWith(".tmp"));
      assert.deepEqual(left, []);
      assert.equal(statSync(join(r.authDir, "token", "oauth-token")).isDirectory(), true); // nothing was replaced
    } finally {
      clean(r);
    }
  });
});
