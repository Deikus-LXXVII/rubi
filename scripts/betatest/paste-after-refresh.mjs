import { chromium, webkit } from "playwright";
import path from "node:path";
import fs from "node:fs";

// Real setup page + fake OAuth, with the same time away a person needs to sign in.
// No reload, Refresh click, or fresh link is allowed between opening OAuth and pasting.
export async function checkPasteAfterRefresh({ mcp, out, pub }) {
  const results = [];
  for (const [engine, type] of Object.entries({ chromium, webkit })) {
    let browser;
    try {
      browser = await type.launch({ headless: true, ...(engine === "chromium" ? { args: ["--no-sandbox"] } : {}) });
    } catch {
      results.push({ name: `${engine}: browser available`, ok: false, detail: "browser could not launch; install Playwright browser and OS dependencies" });
      continue;
    }
    try {
      for (const viewport of [{ width: 1280, height: 800 }, { width: 393, height: 852 }]) {
        const name = `${engine} ${viewport.width}x${viewport.height}: paste after idle and OAuth`;
        const context = await browser.newContext({ viewport, ignoreHTTPSErrors: true });
        const page = await context.newPage();
        const errors = [];
        let phase = "initial prompt";
        let idle = "";
        page.on("pageerror", (e) => errors.push(e.message));
        try {
          await page.goto(mcp("rubi_link", { purpose: "setup:t3agents" }).url);
          const top = page.locator(".view-section", { has: page.getByRole("heading", { name: "Setup", exact: true }) });
          const paste = top.getByRole("button", { name: "Paste the code", exact: true });
          await paste.waitFor();
          // Immediate prompt, before a timer fires, isolates browser/prompt failures.
          await paste.click();
          await page.getByRole("dialog").locator("input").waitFor({ timeout: 2000 });
          await page.getByRole("button", { name: "Cancel", exact: true }).click();
          phase = "OAuth popup and return";
          await page.getByRole("button", { name: "Open the sign-in page", exact: true }).click();
          const sheet = page.getByRole("dialog", { name: "Sign-in link" });
          const popupPromise = context.waitForEvent("page");
          await sheet.getByRole("link", { name: "Open the sign-in page" }).click();
          const popup = await popupPromise;
          await popup.waitForLoadState("domcontentloaded");
          if ((await popup.locator("#code").innerText()).trim() !== pub.signin_code) throw new Error("OAuth code mismatch");
          await popup.close();
          await sheet.getByRole("button", { name: "Done", exact: true }).click();
          if (await page.locator(".ask-overlay").count()) throw new Error("sign-in sheet left an overlay");
          // Closing the sheet completes its action and may redraw the page once.
          // Let that settle before measuring an unchanged polling interval.
          await page.waitForTimeout(1000);
          const node = await paste.elementHandle();
          // Two complete 3s refresh periods: waiting for the user must not retire its controls.
          await page.waitForTimeout(7500);
          const sameNode = await paste.evaluate((el, old) => el === old, node);
          const disabled = await paste.isDisabled();
          idle = `unchanged DOM=${sameNode}; disabled=${disabled}`;
          if (!sameNode) throw new Error("fixture redrew; unchanged refresh was not exercised");
          phase = "prompt after unchanged refreshes";
          await paste.click();
          await page.getByRole("dialog").locator("input").waitFor({ timeout: 2000 });
          await page.getByRole("dialog").locator("input").fill("sandbox-cancelled");
          await page.keyboard.press("Escape");
          if (await page.locator(".ask-overlay").count()) throw new Error("Escape did not close prompt");
          if (errors.length) throw new Error("page error during sign-in");
          await paste.click();
          await page.getByRole("dialog").locator("input").waitFor({ timeout: 2000 });
          await page.screenshot({ path: path.join(out, "screenshots", `paste-${engine}-${viewport.width}.png`) });
          await page.getByRole("button", { name: "Cancel", exact: true }).click();
          results.push({ name, ok: true, detail: `unchanged DOM=${sameNode}; disabled=${disabled}; popup, prompt, Escape, reopen passed` });
        } catch (e) {
          const overlays = await page.locator(".ask-overlay").count();
          const disabled = await page.getByRole("button", { name: "Paste the code", exact: true }).first().isDisabled().catch(() => null);
          results.push({ name, ok: false, detail: `${phase}: ${e.message.split("\n")[0]}; ${idle}; overlays=${overlays}; disabled=${disabled}; page errors=${errors.length}` });
        } finally {
          await context.close();
        }
      }
    } finally {
      await browser.close();
    }
  }
  fs.writeFileSync(path.join(out, "paste-refresh.json"), JSON.stringify(results, null, 2) + "\n");
  return results;
}
