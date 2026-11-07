// Drives the sandbox panel at a phone size and a desktop size.
// Screenshots and the summary go to BETATEST_OUT. Links and pairing codes are not written into the summary.

import { execFileSync } from "node:child_process";
import { execSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { inflateSync } from "node:zlib";
import { chromium } from "playwright";

const dir = process.env.RUBI_SANDBOX_DIR;
const repo = process.env.RUBI_REPO;
const out = process.env.BETATEST_OUT;
if (!dir || !out) {
  console.error("BETATEST_OUT and RUBI_SANDBOX_DIR are required");
  process.exit(2);
}
fs.mkdirSync(path.join(out, "screenshots"), { recursive: true });

const pub = JSON.parse(fs.readFileSync(path.join(dir, "run/public.json"), "utf8"));
const plugin = JSON.parse(fs.readFileSync(path.join(dir, "plugin.json"), "utf8"));
const realPlugin = String(plugin.source || "").includes("github.com");
const password = "sandbox-password-ok";
const shots = [];
const flows = [];
const checks = [];
const consoleErrors = [];

function addCheck(name, ok, detail = "") {
  checks.push({ name, status: ok ? "pass" : "fail", detail: detail || "" });
  return ok;
}

function redact(s) {
  return String(s)
    .replace(/rubi-home:[A-Za-z0-9_-]+/g, "rubi-home:[redacted]")
    .replace(/(#|&|\?)[^\s"'<>]*\b[ptk]=[^&\s"'<>]+/g, "$1[redacted]");
}

function record(flow) {
  flows.push(flow);
  const tag = flow.status.toUpperCase();
  console.log(`${tag} ${flow.name}${flow.error ? " — " + redact(flow.error) : ""}${flow.reason ? " — " + flow.reason : ""}`);
}

// pngNearUniform samples a PNG and reports whether it is one flat colour.
// A shot taken in the middle of a view transition comes out that way.
function pngNearUniform(buf) {
  if (buf.length < 8 || buf.toString("ascii", 1, 4) !== "PNG") return false;
  let o = 8;
  let width = 0;
  let height = 0;
  let bitDepth = 0;
  let colorType = 0;
  const idat = [];
  while (o + 12 <= buf.length) {
    const len = buf.readUInt32BE(o);
    const type = buf.toString("ascii", o + 4, o + 8);
    const data = buf.subarray(o + 8, o + 8 + len);
    if (type === "IHDR") {
      width = data.readUInt32BE(0);
      height = data.readUInt32BE(4);
      bitDepth = data[8];
      colorType = data[9];
    } else if (type === "IDAT") {
      idat.push(data);
    } else if (type === "IEND") {
      break;
    }
    o += 12 + len;
  }
  const channels = { 2: 3, 6: 4 }[colorType];
  if (!channels || bitDepth !== 8 || !width || !height) return false;
  const raw = inflateSync(Buffer.concat(idat));
  const stride = width * channels;
  let p = 0;
  let min = [255, 255, 255];
  let max = [0, 0, 0];
  let prev = null;
  const stepX = Math.max(1, Math.floor(width / 24));
  const stepY = Math.max(1, Math.floor(height / 36));
  for (let y = 0; y < height; y++) {
    const filter = raw[p++];
    const row = Buffer.alloc(stride);
    for (let i = 0; i < stride; i++) {
      const x = raw[p++];
      const a = i >= channels ? row[i - channels] : 0;
      const b = prev ? prev[i] : 0;
      const c = prev && i >= channels ? prev[i - channels] : 0;
      let v = x;
      if (filter === 1) v = (x + a) & 255;
      else if (filter === 2) v = (x + b) & 255;
      else if (filter === 3) v = (x + ((a + b) >> 1)) & 255;
      else if (filter === 4) {
        const pa = Math.abs(b - c);
        const pb = Math.abs(a - c);
        const pc = Math.abs(a + b - 2 * c);
        const pr = pa <= pb && pa <= pc ? a : pb <= pc ? b : c;
        v = (x + pr) & 255;
      }
      row[i] = v;
    }
    prev = row;
    if (y % stepY !== 0) continue;
    for (let x = 0; x < width; x += stepX) {
      const i = x * channels;
      for (let c = 0; c < 3; c++) {
        const n = row[i + c];
        if (n < min[c]) min[c] = n;
        if (n > max[c]) max[c] = n;
      }
    }
  }
  const spread = Math.max(max[0] - min[0], max[1] - min[1], max[2] - min[2]);
  return spread < 18;
}

// settle waits out the finite entrance (the view transition and the card rising).
// The mascot's idle animation never ends, so it is not waited on.
async function settle(page) {
  await page.waitForFunction(() => {
    const skip = new Set(["breathe", "breathe-deep", "sway", "aurora", "progress", "shimmer", "pulse", "dots"]);
    return !document.getAnimations().some((a) => {
      const target = a.effect && a.effect.target;
      if (target && target.closest && target.closest(".mascot")) return false;
      const name = a.animationName || "";
      if (!name || skip.has(name)) return false;
      const iterations = a.effect && a.effect.getTiming ? a.effect.getTiming().iterations : 1;
      return iterations !== Infinity && a.playState === "running";
    });
  }, { timeout: 2000 }).catch(() => {});
  await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
}

async function shot(page, name) {
  await settle(page);
  const file = path.join(out, "screenshots", name + ".png");
  const buf = await page.screenshot({ path: file, fullPage: true, animations: "disabled" });
  shots.push({ name, file });
  let uniform = false;
  try {
    uniform = pngNearUniform(buf);
  } catch (err) {
    addCheck(`screenshot ${name}: not a solid colour`, false, err.message);
    throw err;
  }
  addCheck(`screenshot ${name}: not a solid colour`, !uniform, uniform ? "the image is one flat colour" : "");
  if (uniform) throw new Error(`screenshot ${name} is a solid colour`);
  return file;
}

async function phoneChecks(page, where) {
  const report = await page.evaluate(() => {
    const doc = document.documentElement;
    const overflow = doc.scrollWidth > doc.clientWidth + 1;
    const buttons = [...document.querySelectorAll("button, a.btn, .note-fold summary, .signin-full-wrap summary, .setup-fold > summary")].map((el) => {
      const r = el.getBoundingClientRect();
      const text = (el.innerText || el.getAttribute("aria-label") || "").trim().slice(0, 80);
      return { text, w: Math.round(r.width), h: Math.round(r.height), visible: r.width > 0 && r.height > 0 && getComputedStyle(el).visibility !== "hidden" };
    });
    const h1 = document.querySelector(".card h1")?.innerText || "";
    return { overflow, buttons, h1, scrollWidth: doc.scrollWidth, clientWidth: doc.clientWidth };
  });
  addCheck(`${where}: no horizontal overflow`, !report.overflow, report.overflow ? `${report.scrollWidth} > ${report.clientWidth}` : "");
  const small = report.buttons.filter((b) => b.visible && (b.h < 44 || (b.w < 44 && b.w > 0 && b.text.length < 12)));
  addCheck(`${where}: tap targets at least 44px`, small.length === 0, small.map((b) => `${b.text || "(unlabeled)"} is ${b.w}x${b.h}`).join("; "));
  const errorPage = /Something.s wrong/.test(report.h1);
  addCheck(`${where}: not an error page`, !errorPage, errorPage ? report.h1 : "");
  return report;
}

async function noRepeatedHeadings(page, where) {
  const dup = await page.evaluate(() => [...document.querySelectorAll(".view-section")].filter((sec) => {
    const head = (sec.querySelector("h2")?.childNodes?.[0]?.textContent || "").trim();
    return head && [...sec.querySelectorAll(".view-text strong")].some((s) => s.textContent.trim().startsWith(head));
  }).map((sec) => (sec.querySelector("h2")?.childNodes?.[0]?.textContent || "").trim()));
  addCheck(`${where}: step heading shown once`, dup.length === 0, dup.join("; "));
  if (dup.length) throw new Error("heading repeated in the card: " + dup.join("; "));
}

async function approveWithPassword(page) {
  const pw = page.locator("input[type=password]").last();
  await pw.waitFor({ timeout: 20000 });
  await pw.fill(password);
  await page.locator("button.primary").last().click();
  await page.getByRole("heading", { name: /Sent|Saved|Done|Installed|Updated|routine|connected/ }).waitFor({ timeout: 40000 });
}

const bin = path.join(dir, "bin/rubi-sandbox");
const socket = path.join(dir, "rubi/rubi.sock");

function mcp(tool, args) {
  const dest = path.join(dir, "run", `mcp-${tool}.json`);
  execFileSync(bin, ["mcp", "--socket", socket, "--tool", tool, "--args", JSON.stringify(args ?? {}), "--out", dest], { timeout: 30000 });
  const body = JSON.parse(fs.readFileSync(dest, "utf8"));
  fs.rmSync(dest, { force: true });
  if (body.is_error) throw new Error(redact(body.text || "mcp error"));
  return body.structured || {};
}

let ref = "";
try {
  ref = execSync("git rev-parse HEAD", { cwd: repo, encoding: "utf8" }).trim();
} catch {
  ref = "unknown";
}

const browser = await chromium.launch({
  headless: true,
  args: ["--no-sandbox", "--disable-dev-shm-usage"],
});
const context = await browser.newContext({
  viewport: { width: 393, height: 852 },
  isMobile: true,
  hasTouch: true,
  ignoreHTTPSErrors: true,
  deviceScaleFactor: 2,
  permissions: ["clipboard-read", "clipboard-write"],
});
const page = await context.newPage();
page.on("pageerror", (err) => consoleErrors.push(redact(err.message)));
page.on("console", (msg) => {
  if (msg.type() === "error") consoleErrors.push(redact(msg.text()));
});

async function flow(name, fn, opts = {}) {
  if (opts.needsReal && !realPlugin) {
    record({ name, status: "blocked", reason: "the T3 Agents plugin from GitHub was not built; the in-repo stand-in is not treated as that plugin" });
    return;
  }
  try {
    await fn();
    const h1 = await cardTitle(page).catch(() => "");
    if (/Something.s wrong/.test(h1) && !opts.allowError) {
      throw new Error("page says Something's wrong: " + h1);
    }
    record({ name, status: "pass" });
  } catch (err) {
    await shot(page, "fail-" + name.replace(/[^a-z0-9]+/gi, "-").slice(0, 40)).catch(() => {});
    record({ name, status: "fail", error: err.message });
  }
}

const status = JSON.parse(fs.readFileSync(path.join(dir, "run/status.json"), "utf8"));
const pairLink = status.structured?.link;
if (!pairLink) {
  record({ name: "pair link", status: "fail", error: "rubi_status did not include a link" });
} else {
  await flow("pair with a password", async () => {
    await page.goto(pairLink);
    await page.getByRole("button", { name: "Use a password only" }).click();
    await page.locator("input[type=password]").nth(0).fill(password);
    await page.locator("input[type=password]").nth(1).fill(password);
    await shot(page, "01-pair-password");
    await page.getByRole("button", { name: /Set password/ }).click();
    await page.getByRole("heading", { name: /Connect your agent/ }).waitFor({ timeout: 90000 });
    await shot(page, "02-paired");
    await page.getByText("I have the routine open here").click();
    await page.getByPlaceholder("The Grok Bot's name*").fill("Sandbox");
    if (!pub.webhook_url) throw new Error("sandbox did not publish a webhook stub");
    await page.locator("textarea").fill(`${pub.webhook_url} sandbox-webhook-key-ok`);
    await page.getByRole("button", { name: "Connect", exact: true }).click();
    await approveWithPassword(page);
    await page.getByRole("heading", { name: /is connected/ }).waitFor({ timeout: 40000 });
    const connected = await cardTitle(page);
    if (/didn.t take the test|No answer from the routine/.test(connected)) throw new Error(connected);
    await shot(page, "02b-agent");
    await phoneChecks(page, "after pair");
  });
}

async function cardTitle(page) {
  const h = page.locator(".card h1");
  if (!(await h.count())) return "";
  return (await h.first().innerText()).trim();
}

async function backToSettings(page) {
  if ((await cardTitle(page)) === "Settings") return;
  const done = page.getByRole("button", { name: "Back to settings" });
  if (await done.count()) {
    await done.click();
    return;
  }
  const back = page.locator("button.back");
  if (await back.count()) {
    await back.first().click();
    return;
  }
  const gear = page.locator("button.settings-btn");
  if (await gear.count()) await gear.click();
  await page.locator(".card h1", { hasText: "Settings" }).waitFor({ timeout: 15000 });
}

await flow("open settings and pair the fake Home", async () => {
  await page.getByRole("button", { name: "Settings" }).click();
  await page.getByRole("heading", { name: "Plugins" }).waitFor();
  await shot(page, "03-plugins-empty");
  await page.getByRole("button", { name: "Rubi Home" }).click();
  await page.getByRole("button", { name: "Add a home computer" }).click();
  await page.getByRole("heading", { name: "Add a home computer" }).waitFor();
  await shot(page, "04-add-home-before-code");
  const code = fs.readFileSync(path.join(dir, "run/home-code"), "utf8").trim();
  await page.locator("textarea").fill(code);
  await page.getByRole("button", { name: "Pair", exact: true }).click();
  await approveWithPassword(page);
  await shot(page, "05-home-paired");
  await backToSettings(page);
  await page.getByRole("button", { name: "Plugins" }).waitFor();
});

await flow("install T3 Agents from the local catalog", async () => {
  await backToSettings(page);
  await page.getByRole("button", { name: "Plugins" }).click();
  await page.getByRole("button", { name: "Add from the store" }).click();
  await page.getByText("T3 Agents").first().waitFor({ timeout: 20000 });
  await shot(page, "06-store");
  const card = page.locator("article", { hasText: "T3 Agents" });
  await card.getByRole("button", { name: "Install" }).click();
  await approveWithPassword(page);
  await page.getByRole("heading", { name: /Installed|Saved|Done/ }).waitFor({ timeout: 30000 });
  await shot(page, "07-installed");
});

await flow("plugins list and open the plugin page", async () => {
  const backSettings = page.getByRole("button", { name: "Back to settings" });
  if (await backSettings.count()) await backSettings.click();
  if ((await cardTitle(page)) !== "Settings") {
    await page.locator("button.back").first().click();
  }
  await page.getByRole("heading", { name: "Settings", exact: true }).waitFor();
  await page.getByRole("button", { name: "Plugins" }).click();
  const open = page.getByRole("button", { name: /^Open / });
  await open.waitFor();
  await shot(page, "08-plugins-list");
  await open.click();
  await page.waitForFunction(() => {
    const t = document.querySelector(".card h1")?.innerText?.trim() || "";
    return t && t !== "Settings" && t !== "Plugins";
  }, { timeout: 20000 });
  const h1 = await cardTitle(page);
  if (/Something.s wrong/.test(h1)) throw new Error(h1);
  if (h1 !== "T3 Agents") throw new Error("plugin page title is " + JSON.stringify(h1) + "; the view still says Claude Code, and the page should use the plugin name");
  const visible = await page.locator(".card").innerText();
  if (/\bbypassPermissions\b/.test(visible)) throw new Error("plugin page shows bypassPermissions in the open text");
  if (await page.locator(".note-fold").count()) {
    if (!(await page.locator(".note-fold summary", { hasText: "Details" }).count())) throw new Error("long note is not collapsible");
  }
  await shot(page, "09-plugin-page");
  await phoneChecks(page, "plugin page");
});

await flow("approval link, then the plugin page still opens", async () => {
  const res = mcp("rubi_dev_request_approval", { to: "ada@example.com", subject: "Sandbox", body: "Hello from the sandbox." });
  const url = res.approval_url;
  if (!url) throw new Error("no approval_url");
  await page.goto(url);
  await page.getByRole("heading", { name: /Send this email/ }).waitFor();
  await shot(page, "10-approval");
  await approveWithPassword(page);
  await page.getByRole("heading", { name: /Sent|Done|Saved/ }).waitFor({ timeout: 30000 });
  await shot(page, "11-approval-done");
  await backToSettings(page);
  await page.getByRole("button", { name: "Plugins" }).click().catch(() => {});
  await page.getByRole("button", { name: /^Open / }).click();
  await page.locator(".card h1").first().waitFor();
  const h1 = await cardTitle(page);
  if (/Something.s wrong/.test(h1)) throw new Error("bug b: " + h1);
  await shot(page, "12-plugin-after-approval");
  await phoneChecks(page, "plugin after approval");
});

await flow("setup link opens the setup page", async () => {
  const res = mcp("rubi_link", { purpose: "setup:t3agents" });
  if (!res.url) throw new Error("no setup link");
  await page.goto(res.url);
  await page.getByRole("heading", { name: /Set up T3 Agents/ }).waitFor({ timeout: 20000 });
  const h1 = await page.getByRole("heading", { name: /Set up T3 Agents/ }).innerText();
  if (/^Connect /.test(h1) || /^Saved$/.test(h1) || /Something.s wrong/.test(h1)) {
    throw new Error("bug a: setup link showed " + h1);
  }
  if (!/Set up T3 Agents/.test(h1)) throw new Error("expected the setup page, saw " + h1);
  const back = (await page.locator("button.back").innerText()).replace(/\s+/g, " ").trim();
  if (/Claude Code/.test(back)) throw new Error("setup back link says " + JSON.stringify(back));
  if (!/T3 Agents/.test(back)) throw new Error("setup back link should name the plugin, saw " + JSON.stringify(back));
  const setupAll = page.getByRole("button", { name: "Set up everything", exact: true });
  await setupAll.waitFor();
  const setupCls = await setupAll.getAttribute("class");
  const primaryOk = /primary/.test(setupCls || "") && !/secondary/.test(setupCls || "");
  addCheck("Set up everything is the only primary action", primaryOk && (await page.getByRole("button", { name: "Start", exact: true }).count()) === 0, setupCls || "");
  if (!primaryOk) throw new Error("Set up everything is not the primary button");
  if (await page.getByRole("button", { name: "Start", exact: true }).count()) throw new Error("Start competes with Set up everything");
  if (/appears once the container/.test(await page.locator(".card").innerText())) throw new Error("the Set up everything hint contradicts the button");
  const gap = await page.evaluate(() => {
    const sum = document.querySelector(".note-more summary");
    const next = document.querySelector(".view-section, .setup-fold");
    if (!sum || !next) return -1;
    return next.getBoundingClientRect().top - sum.getBoundingClientRect().bottom;
  });
  addCheck("collapsed Details has no empty gap", gap >= -2 && gap <= 32, `gap ${Math.round(gap)}px`);
  if (gap > 32) throw new Error("collapsed Details leaves a gap of " + Math.round(gap) + "px");
  await noRepeatedHeadings(page, "setup link");
  await shot(page, "13-setup-link");
  const body = await page.locator(".card").innerText();
  if (!/Waiting for earlier steps/.test(body)) {
    throw new Error("bug d: login step does not say it is waiting for earlier steps");
  }
  await phoneChecks(page, "setup link");
}, { needsReal: true });

await flow("T3 setup steps advance", async () => {
  await page.getByRole("button", { name: "Set up everything", exact: true }).click();
  // The plugin reuses a status it just read for a couple of seconds.
  await page.waitForTimeout(2500);
  const refresh = page.getByRole("button", { name: "Refresh", exact: true });
  if (await refresh.count()) await refresh.click();
  for (const heading of [/Container service/, /Runner image/, /T3 installed and pinned/, /T3 paired with Rubi/]) {
    const sec = page.locator(".view-section", { has: page.getByRole("heading", { name: heading }) });
    try {
      await sec.locator(".status-badge", { hasText: /^done$/ }).waitFor({ timeout: 15000 });
    } catch (err) {
      const text = await page.locator(".card").innerText();
      const errText = await page.locator(".error").allInnerTexts().catch(() => []);
      throw new Error(String(heading) + " did not finish. " + errText.join(" | ") + " Page: " + text.slice(0, 700));
    }
  }
  await shot(page, "14-setup-all");
  await page.getByRole("button", { name: "Sign in", exact: true }).first().evaluate((el) => el.click());
  await page.waitForTimeout(2500);
  if (await refresh.count()) await refresh.click();
  await page.getByRole("button", { name: "Open the sign-in page" }).waitFor({ timeout: 20000 });
  const waitingCard = await page.locator(".card").innerText();
  if (/https?:\/\//.test(waitingCard)) throw new Error("a setup row prints the raw sign-in URL");
  if (/Sign-in waiting for the browser/i.test(waitingCard)) throw new Error("duplicate sign-in waiting row");
  const loginSec = page.locator(".view-section", { has: page.getByRole("heading", { name: /Provider sign-in/ }) });
  if (await loginSec.locator(".view-text strong").count()) throw new Error("a single role is shown as its own row");
  await shot(page, "18-signin-waiting");
  await phoneChecks(page, "sign-in waiting");
}, { needsReal: true });

await flow("open and copy the full sign-in link on the phone", async () => {
  await page.getByRole("button", { name: "Open the sign-in page" }).click();
  const sheet = page.getByRole("dialog", { name: "Sign-in link" });
  await sheet.waitFor({ timeout: 10000 });
  const short = sheet.locator(".signin-short");
  const shown = (await short.innerText()).trim();
  const stored = await short.getAttribute("data-full-url");
  const full = await sheet.locator("textarea.signin-full").inputValue();
  const show = sheet.locator("summary", { hasText: "Show the full link" });
  const showBox = await show.boundingBox();
  addCheck("Show the full link is a 44px disclosure", !!(showBox && showBox.height >= 44 && showBox.width >= 44), showBox ? `${Math.round(showBox.width)}x${Math.round(showBox.height)}` : "missing");
  if (!showBox || showBox.height < 44 || showBox.width < 44) throw new Error("Show the full link is not a 44px target");
  addCheck("sign-in link: short form on screen", shown.length < 80 && shown !== pub.signin_url && shown.includes("…"), shown);
  addCheck("sign-in link: full URL kept for copy", stored === pub.signin_url && full === pub.signin_url, stored === pub.signin_url ? "" : "stored link differs");
  if (shown.length >= 80 || shown === pub.signin_url || !shown.includes("…")) throw new Error("the sign-in link is shown as a wall of text: " + shown.slice(0, 80));
  if (stored !== pub.signin_url || full !== pub.signin_url) throw new Error("the sheet does not hold the full sign-in URL");
  const actions = await sheet.locator(".signin-actions").locator("a, button").allInnerTexts();
  const open = sheet.getByRole("link", { name: "Open the sign-in page" });
  const openCls = await open.getAttribute("class");
  const doneCls = await sheet.getByRole("button", { name: "Done" }).getAttribute("class");
  addCheck("sign-in link: Open is the single primary action", /primary/.test(openCls || "") && !/primary/.test(doneCls || "") && actions[0].includes("Open the sign-in page"), actions.join(" | "));
  if (!/primary/.test(openCls || "") || /primary/.test(doneCls || "")) throw new Error("Open and Done are not primary then neutral");
  if (!actions[0].includes("Open the sign-in page")) throw new Error("Open is not the first action: " + actions.join(" | "));
  const box = await open.boundingBox();
  const onScreen = !!(box && box.y + box.height < 852);
  addCheck("sign-in link: Open is on the phone screen", onScreen, box ? `y=${Math.round(box.y)}` : "no box");
  if (!onScreen) throw new Error("Open the sign-in page is below the phone screen");
  await phoneChecks(page, "sign-in link");
  await shot(page, "19-signin-link");
  await page.getByRole("button", { name: "Copy the link" }).click();
  const copied = await page.evaluate(async () => {
    try { return await navigator.clipboard.readText(); } catch { return ""; }
  });
  const selected = await page.evaluate(() => (document.activeElement && document.activeElement.value) || "");
  const copyOk = copied === pub.signin_url || selected === pub.signin_url;
  addCheck("sign-in link: Copy copies the full URL", copyOk, copyOk ? "" : "clipboard and the field differ from the full URL");
  if (!copyOk) throw new Error("Copy did not provide the full sign-in URL");
  await open.click();
  await page.waitForTimeout(500);
  const pages = context.pages();
  const opened = pages[pages.length - 1];
  await opened.waitForLoadState("domcontentloaded").catch(() => {});
  const code = await opened.locator("#code").innerText().catch(() => "");
  if (code.trim() !== pub.signin_code) throw new Error("the sign-in page did not show the sandbox code");
  await shot(opened, "20-oauth");
  if (opened !== page) await opened.close();
  await page.getByRole("button", { name: "Done" }).click();
}, { needsReal: true });

await flow("a wrong code fails the sign-in and can be retried", async () => {
  await page.getByRole("button", { name: "Paste the code" }).click();
  await page.getByRole("dialog").locator("input").fill("not-the-sandbox-code");
  await page.getByRole("button", { name: "OK" }).click();
  await page.getByText("Signing in...").waitFor({ timeout: 25000 });
  await page.getByText("The sign-in failed: that code was not accepted.").waitFor({ timeout: 25000 });
  await page.getByRole("button", { name: "Sign in", exact: true }).waitFor();
  await shot(page, "21b-wrong-code");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await page.waitForTimeout(2500);
  const retry = mcp("rubi_link", { purpose: "setup:t3agents" });
  if (!retry.url) throw new Error("no setup link");
  await page.goto(retry.url);
  await page.getByRole("button", { name: "Open the sign-in page" }).waitFor({ timeout: 20000 });
}, { needsReal: true });

await flow("paste the code", async () => {
  await page.getByRole("button", { name: "Paste the code" }).click();
  await page.getByRole("dialog").locator("input").fill(pub.signin_code);
  await shot(page, "21-paste-code");
  await page.getByRole("button", { name: "OK" }).click();
  await page.getByText("Signing in...").waitFor({ timeout: 25000 });
  const setupBox = page.locator(".view-section", { has: page.getByRole("heading", { name: "Setup", exact: true }) });
  await setupBox.getByText("Signing in to Claude").waitFor();
  await setupBox.getByRole("button", { name: "Cancel signing in" }).waitFor();
  const foldedWhile = await page.locator(".view-section h2", { hasText: "Apple container" }).isVisible().catch(() => false);
  addCheck("finished steps fold while a step runs", foldedWhile === false, foldedWhile ? "Apple container is still on screen" : "");
  if (foldedWhile) throw new Error("finished steps stay open while signing in");
  const spin = await page.evaluate(() => getComputedStyle(document.querySelector(".status-badge.status-working"), "::before").animationName);
  addCheck("working badge spins", spin === "spin", spin || "no spinner");
  if (spin !== "spin") throw new Error("the working badge does not spin");
  await page.emulateMedia({ reducedMotion: "reduce" });
  const still = await page.evaluate(() => getComputedStyle(document.querySelector(".status-badge.status-working"), "::before").animationName);
  addCheck("working badge is still when motion is reduced", still === "none", still || "");
  await page.emulateMedia({ reducedMotion: "no-preference" });
  if (still !== "none") throw new Error("the working badge keeps spinning when motion is reduced");
  await shot(page, "22-signing-in");
  await page.getByText("Running the self-test.").waitFor({ timeout: 25000 });
  await setupBox.getByText("Running the self-test").waitFor();
  await setupBox.getByRole("button", { name: "Cancel the self-test" }).waitFor();
  await shot(page, "22b-selftest");
  const banner = page.locator(".ready-banner");
  await banner.waitFor({ timeout: 25000 });
  await page.getByText(/Setup steps \(7\/7 done\)/).waitFor();
  await page.getByRole("button", { name: "Run the self-test again" }).waitFor();
  if (await page.getByRole("button", { name: "Run self-test", exact: true }).count()) {
    throw new Error("sign-in still asks for a separate self-test");
  }
  if (await page.getByRole("button", { name: "Set up everything", exact: true }).count()) {
    throw new Error("Set up everything is still offered after the steps it runs are done");
  }
  const signedGap = await page.evaluate(() => {
    const sum = document.querySelector(".note-more summary");
    const next = document.querySelector(".setup-fold, .view-section");
    if (!sum || !next) return -1;
    return next.getBoundingClientRect().top - sum.getBoundingClientRect().bottom;
  });
  addCheck("collapsed Details has no empty gap after sign-in", signedGap >= -2 && signedGap <= 32, `gap ${Math.round(signedGap)}px`);
  if (signedGap > 32) throw new Error("collapsed Details leaves a gap of " + Math.round(signedGap) + "px");
  await phoneChecks(page, "signed in");
}, { needsReal: true });

await flow("self-test ran with sign-in and can be run again", async () => {
  const banner = page.locator(".ready-banner");
  await banner.waitFor();
  await page.evaluate(() => window.scrollTo(0, 0));
  const box = await banner.boundingBox();
  const inView = !!(box && box.y >= 0 && box.y + box.height <= 852);
  addCheck("ready banner is in the first phone viewport", inView, box ? `y=${Math.round(box.y)} h=${Math.round(box.height)}` : "missing");
  if (!inView) throw new Error("the ready banner is not in the first phone viewport");
  if (!/T3 Agents is ready/.test(await banner.innerText())) throw new Error("ready banner does not name the plugin");
  if (await page.getByRole("button", { name: "Set up everything", exact: true }).count()) {
    throw new Error("Set up everything is still offered");
  }
  if (await page.getByRole("button", { name: "Refresh", exact: true }).count()) {
    throw new Error("Refresh is still offered when every step is done");
  }
  if (/Set up everything/.test(await page.locator(".card").innerText())) {
    throw new Error("the Set up everything explanation is still shown");
  }
  const folded = await page.locator(".setup-fold .view-section h2").first().isVisible().catch(() => false);
  addCheck("finished steps are collapsed", folded === false, folded ? "a step heading is visible" : "");
  if (folded) throw new Error("finished steps are still expanded");
  const readyGap = await page.evaluate(() => {
    const sum = document.querySelector(".note-more summary");
    const next = document.querySelector(".setup-fold");
    if (!sum || !next) return -1;
    return next.getBoundingClientRect().top - sum.getBoundingClientRect().bottom;
  });
  addCheck("collapsed Details has no empty gap on the ready screen", readyGap >= -2 && readyGap <= 32, `gap ${Math.round(readyGap)}px`);
  if (readyGap > 32) throw new Error("collapsed Details leaves a gap of " + Math.round(readyGap) + "px");
  await page.getByRole("button", { name: "Run the self-test again" }).click();
  await page.locator(".ready-banner", { hasText: "is ready" }).waitFor({ timeout: 25000 });
  await page.getByText(/Setup steps \(7\/7 done\)/).waitFor({ timeout: 25000 });
  await shot(page, "23-ready");
  await phoneChecks(page, "ready");
  await noRepeatedHeadings(page, "ready");
}, { needsReal: true });

await flow("a failed self-test explains itself and can be retried", async () => {
  await page.locator(".setup-fold > summary").click();
  await page.getByRole("button", { name: "Sign in again" }).click();
  await page.waitForTimeout(2500);
  const again = mcp("rubi_link", { purpose: "setup:t3agents" });
  if (!again.url) throw new Error("no setup link");
  await page.goto(again.url);
  await page.getByRole("button", { name: "Paste the code" }).waitFor({ timeout: 20000 });
  await page.getByRole("button", { name: "Paste the code" }).click();
  await page.getByRole("dialog").locator("input").fill("sandbox-selftest-fail");
  await page.getByRole("button", { name: "OK" }).click();
  await page.getByText("The self-test failed: the role VM did not start.").waitFor({ timeout: 30000 });
  await page.getByRole("button", { name: "Run self-test" }).waitFor();
  await shot(page, "25-selftest-failed");
  await page.getByRole("button", { name: "Run self-test" }).click();
  await page.waitForTimeout(2500);
  const after = mcp("rubi_link", { purpose: "setup:t3agents" });
  await page.goto(after.url);
  await page.locator(".ready-banner").waitFor({ timeout: 30000 });
}, { needsReal: true });

await flow("desktop viewport of the setup page", async () => {
  // The panel removes the one-time link from the address bar, so a reload of that URL is the
  // landing page. A fresh setup link is what a person asks their agent for.
  const res = mcp("rubi_link", { purpose: "setup:t3agents" });
  if (!res.url) throw new Error("no setup link");
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto(res.url);
  await page.getByRole("heading", { name: /Set up T3 Agents/ }).waitFor({ timeout: 20000 });
  await shot(page, "24-desktop");
  const desk = await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth + 1);
  addCheck("desktop: no horizontal overflow", !desk, desk ? "desktop viewport overflows" : "");
  await page.goBack().catch(() => {});
}, { needsReal: true });

await browser.close();

const failed = flows.filter((f) => f.status === "fail");
const passed = flows.filter((f) => f.status === "pass");
const blocked = flows.filter((f) => f.status === "blocked");
const checkFails = checks.filter((c) => c.status === "fail");
const summary = {
  ref,
  plugin,
  viewport: { phone: "393x852", desktop: "1280x800" },
  flows,
  checks,
  console_errors: consoleErrors.slice(0, 30),
  screenshots: shots.map((s) => path.basename(s.file)),
  counts: {
    pass: passed.length,
    fail: failed.length,
    blocked: blocked.length,
    checks_pass: checks.length - checkFails.length,
    checks_fail: checkFails.length,
  },
};
fs.writeFileSync(path.join(out, "summary.json"), JSON.stringify(summary, null, 2) + "\n");
const md = [
  `# Rubi sandbox beta test`,
  ``,
  `Source: \`${ref}\``,
  `Plugin: ${plugin.source}`,
  ``,
  `## Flows`,
  ...flows.map((f) => `- **${f.status}** ${f.name}${f.error ? `: ${redact(f.error)}` : ""}${f.reason ? ` (${f.reason})` : ""}`),
  ``,
  `## Checks`,
  ...(checks.length ? checks.map((c) => `- **${c.status}** ${c.name}${c.detail ? `: ${redact(c.detail)}` : ""}`) : ["- No check ran."]),
  ``,
  `## Console`,
  ...(consoleErrors.length ? consoleErrors.slice(0, 20).map((e) => `- ${e}`) : ["- No page errors."]),
  ``,
  `Screenshots: ${shots.length}`,
].join("\n");
fs.writeFileSync(path.join(out, "summary.md"), md + "\n");

if (failed.length || checkFails.length) process.exit(1);
