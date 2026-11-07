// Drives the sandbox panel at a phone size and a desktop size.
// Screenshots and the summary go to BETATEST_OUT. Links and pairing codes are not written into the summary.

import { execFileSync } from "node:child_process";
import { execSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
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
const usability = [];
const consoleErrors = [];

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

async function shot(page, name) {
  const file = path.join(out, "screenshots", name + ".png");
  await page.screenshot({ path: file, fullPage: true });
  shots.push({ name, file });
  return file;
}

async function phoneChecks(page, where) {
  const report = await page.evaluate(() => {
    const doc = document.documentElement;
    const overflow = doc.scrollWidth > doc.clientWidth + 1;
    const buttons = [...document.querySelectorAll("button, a.btn, a.signin-link")].map((el) => {
      const r = el.getBoundingClientRect();
      const text = (el.innerText || el.getAttribute("aria-label") || "").trim().slice(0, 80);
      return { text, w: Math.round(r.width), h: Math.round(r.height), visible: r.width > 0 && r.height > 0 };
    });
    const h1 = document.querySelector("h1")?.innerText || "";
    return { overflow, buttons, h1, scrollWidth: doc.scrollWidth, clientWidth: doc.clientWidth };
  });
  if (report.overflow) {
    usability.push({ where, kind: "horizontal-overflow", detail: `${report.scrollWidth} > ${report.clientWidth}` });
  }
  for (const b of report.buttons) {
    if (!b.visible) continue;
    if (b.h < 44 || (b.w < 44 && b.w > 0 && b.text.length < 12)) {
      usability.push({ where, kind: "tap-target", detail: `${b.text || "(unlabeled)"} is ${b.w}x${b.h}` });
    }
  }
  if (/Something.s wrong/.test(report.h1)) {
    usability.push({ where, kind: "error-page", detail: report.h1 });
  }
  return report;
}

async function approveWithPassword(page) {
  const pw = page.locator("input[type=password]").last();
  await pw.waitFor({ timeout: 20000 });
  await pw.fill(password);
  await page.locator("button.primary").last().click();
  await page.getByRole("heading", { name: /Sent|Saved|Done|Installed|Updated|routine/ }).waitFor({ timeout: 40000 });
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
    await page.locator("textarea").fill("http://127.0.0.1:9/hook sandbox-webhook-key");
    await page.getByRole("button", { name: "Connect", exact: true }).click();
    await approveWithPassword(page);
    await page.getByRole("heading", { name: /routine|Saved|Connect/ }).waitFor({ timeout: 40000 });
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
  await page.locator(".card h1").first().waitFor();
  const h1 = await cardTitle(page);
  if (/Something.s wrong/.test(h1)) throw new Error(h1);
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
  await shot(page, "13-setup-link");
  const body = await page.locator(".card").innerText();
  if (!/Waiting for earlier steps/.test(body)) {
    throw new Error("bug d: login step does not say it is waiting for earlier steps");
  }
  await phoneChecks(page, "setup link");
}, { needsReal: true });

await flow("T3 setup steps advance", async () => {
  const steps = [
    ["Start", "Container service: done", "14-service"],
    ["Build", "Runner image: done", "15-image"],
    ["Install the pinned T3", "T3 installed and pinned: done", "16-t3"],
    ["Pair", "T3 paired with Rubi: done", "17-pair"],
  ];
  for (const [label, done, file] of steps) {
    const btn = page.getByRole("button", { name: label, exact: true });
    await btn.waitFor({ timeout: 20000 });
    await btn.evaluate((el) => el.click());
    // The plugin reuses a status it just read for a couple of seconds, and this page does not
    // refresh on its own once nothing is running. Ask it to read the computer again.
    await page.waitForTimeout(2500);
    const refresh = page.getByRole("button", { name: "Refresh", exact: true });
    if (await refresh.count()) await refresh.click();
    try {
      await page.getByText(done).waitFor({ timeout: 15000 });
    } catch (err) {
      const text = await page.locator(".card").innerText();
      const errText = await page.locator(".error").allInnerTexts().catch(() => []);
      throw new Error(label + " did not finish. " + errText.join(" | ") + " Page: " + text.slice(0, 700));
    }
    await shot(page, file);
  }
  await page.getByRole("button", { name: "Sign in", exact: true }).first().evaluate((el) => el.click());
  await page.waitForTimeout(2500);
  const refresh = page.getByRole("button", { name: "Refresh", exact: true });
  if (await refresh.count()) await refresh.click();
  await page.getByRole("button", { name: "Open the sign-in page" }).waitFor({ timeout: 20000 });
  await shot(page, "18-signin-waiting");
  await phoneChecks(page, "sign-in waiting");
}, { needsReal: true });

await flow("open and copy the full sign-in link on the phone", async () => {
  await page.getByRole("button", { name: "Open the sign-in page" }).click();
  const link = page.locator("a.signin-link");
  await link.waitFor({ timeout: 10000 });
  const href = await link.getAttribute("href");
  const text = await link.innerText();
  if (href !== pub.signin_url) throw new Error("link href is not the full sign-in URL");
  if (text !== pub.signin_url) throw new Error("the visible link is not the full URL");
  if (text.endsWith("…") || text.includes("shortened")) throw new Error("the sign-in link is truncated");
  await shot(page, "19-signin-link");
  const box = await link.boundingBox();
  if (box && box.y + box.height > 852) usability.push({ where: "sign-in link", kind: "below-fold", detail: "link extends past the phone viewport" });
  await page.getByRole("button", { name: "Copy the link" }).click();
  await page.getByRole("link", { name: "Open the sign-in page" }).click();
  await page.waitForTimeout(500);
  const pages = context.pages();
  const opened = pages[pages.length - 1];
  await opened.waitForLoadState("domcontentloaded").catch(() => {});
  const code = await opened.locator("#code").innerText().catch(() => "");
  if (code.trim() !== pub.signin_code) throw new Error("the sign-in page did not show the sandbox code");
  await opened.screenshot({ path: path.join(out, "screenshots", "20-oauth.png"), fullPage: true });
  if (opened !== page) await opened.close();
  await page.getByRole("button", { name: "Done" }).click();
}, { needsReal: true });

await flow("paste the code", async () => {
  await page.getByRole("button", { name: "Paste the code" }).click();
  await page.getByRole("dialog").locator("input").fill(pub.signin_code);
  await shot(page, "21-paste-code");
  await page.getByRole("button", { name: "OK" }).click();
  await page.getByText(/Signed in|done/i).first().waitFor({ timeout: 15000 });
  await shot(page, "22-signed-in");
  await phoneChecks(page, "signed in");
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
  if (desk) usability.push({ where: "desktop", kind: "horizontal-overflow", detail: "desktop viewport overflows" });
  await page.goBack().catch(() => {});
}, { needsReal: true });

await browser.close();

const failed = flows.filter((f) => f.status === "fail");
const passed = flows.filter((f) => f.status === "pass");
const blocked = flows.filter((f) => f.status === "blocked");
const summary = {
  ref,
  plugin,
  viewport: { phone: "393x852", desktop: "1280x800" },
  flows,
  usability,
  console_errors: consoleErrors.slice(0, 30),
  screenshots: shots.map((s) => path.basename(s.file)),
  counts: { pass: passed.length, fail: failed.length, blocked: blocked.length },
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
  `## Usability`,
  ...(usability.length ? usability.map((u) => `- ${u.where}: ${u.kind} — ${u.detail}`) : ["- No overflow or undersized tap target was measured."]),
  ``,
  `## Console`,
  ...(consoleErrors.length ? consoleErrors.slice(0, 20).map((e) => `- ${e}`) : ["- No page errors."]),
  ``,
  `Screenshots: ${shots.length}`,
].join("\n");
fs.writeFileSync(path.join(out, "summary.md"), md + "\n");

if (failed.length) process.exit(1);
