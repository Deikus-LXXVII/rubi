// Rubi panel: pairing, unlocking and approving. A static page; all state lives on the user's Rubi
// instance and, for pinning, in this browser's localStorage.

import { RubiClient, UserError, parseLink } from "./api.js";
import {
  fingerprint, approveKey, b64u, hmacSha256, newPasswordKdf, passwordKek, prfKek, rand, supportsX25519, unwrapDek, wrapDek,
} from "./crypto.js";
import { DEMO_HELLO, DEMO_SCREENS, DemoClient } from "./demo.js";
import { icon } from "./icons.js";
import { celebrate, lookAt, mascot, react, setMood, shy } from "./mascot.js";
import { createPasskey, evalPrf, passkeysAvailable, signChallenge } from "./passkey.js";

const root = document.getElementById("app");

// ---------- tiny DOM helpers (text only: nothing from Rubi or third parties is parsed as HTML) ----------

function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else if (k === "style") el.style.cssText = v; // CSSOM, which the page's CSP allows (style attributes it doesn't)
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

// screen replaces the panel's content. An optional first argument {cls, focus} adds a class to the card
// and controls autofocus (off for screens where a keyboard popping up would get in the way).
// Screens change with a view transition where the browser has one: the old card fades out, the new one's
// blocks rise in turn, and Rubi flies from where it was to where it is now (the "rubi" transition name).
function screen(...children) {
  let opts = {};
  if (children[0] && !(children[0] instanceof Node) && typeof children[0] === "object") opts = children.shift();
  showPanel();
  const main = h("main", { class: "card" + (opts.cls ? " " + opts.cls : "") }, ...children);
  const hero = main.querySelector(".screen-mascot .mascot") || main.querySelector(".mascot");
  const before = root.querySelector("[data-vt=rubi]");
  if (hero) {
    hero.style.viewTransitionName = "rubi";
    hero.dataset.vt = "rubi";
  }
  const viaVT = !!(document.startViewTransition && root.firstChild && !reducedMotion() && !document.hidden);
  const morph = !!(hero && before && viaVT);
  // During a view transition the browser fades the card in itself, and Rubi's flight ends where it stood
  // when the transition began: nothing around Rubi may still be moving then, or it would jump on landing.
  if (viaVT) main.classList.add("vt");
  const swap = () => {
    root.replaceChildren(main);
    window.scrollTo({ top: 0 });
    animateIn(main, morph ? hero : null);
    for (const svg of main.querySelectorAll(".mascot")) svg.enter?.(morph && svg === hero);
    // Focus the first field; otherwise the title, so screen readers announce the new screen.
    const field = opts.focus !== false && root.querySelector("input:not([type=checkbox]):not([type=radio])");
    const title = main.querySelector("h1");
    if (field) field.focus({ preventScroll: true });
    else if (title) {
      title.tabIndex = -1;
      title.focus({ preventScroll: true });
    }
    if (title) document.title = `${title.textContent.trim()} · Rubi`;
  };
  if (viaVT) {
    const vt = document.startViewTransition(swap);
    vt.ready.catch(() => {}); // skipped (e.g. the tab went to the background): the swap still happens
    vt.finished.catch(() => {});
  } else {
    swap();
  }
}

const reducedMotion = () => matchMedia("(prefers-reduced-motion: reduce)").matches;

// Rubi's attention, page-wide: it closes its eyes while you type a password, and looks at the primary
// button you're about to press.
const screenMascot = () => root.querySelector(".screen-mascot .mascot, .req-top .mascot, .instance .mascot");
root.addEventListener("focusin", (e) => { if (e.target.matches?.("input[type=password]")) shy(screenMascot(), true); });
root.addEventListener("focusout", (e) => { if (e.target.matches?.("input[type=password]")) shy(screenMascot(), false); });
root.addEventListener("pointerover", (e) => {
  const b = e.target.closest?.("button.primary");
  if (b && !reducedMotion()) lookAt(screenMascot(), b);
});
root.addEventListener("pointerout", (e) => {
  const b = e.target.closest?.("button.primary");
  if (b && !b.contains(e.relatedTarget)) lookAt(screenMascot(), null);
});

// animateIn staggers the entrance of a screen's blocks.
function animateIn(main, still) {
  const items = [...main.querySelectorAll(":scope > *:not(.req-grid):not(.screen-mascot), .req-grid > section > *, .req-grid > aside > *")]
    .filter((el) => !still || !el.contains(still));
  items.forEach((el, i) => {
    el.classList.add("rise");
    el.style.setProperty("--d", `${Math.min(i, 10) * 40}ms`);
  });
}

// face is the mascot at the top of a screen; its eyes show Rubi's state.
// face is the mascot at the top of a screen. It pops in (or, when it flew in from the last screen, just
// lands), then plays the mood's reaction: the smile and shine when happy, `then` otherwise (a sigh, a shake).
function face(mood, size = 76, then) {
  const wrap = h("div", { class: `screen-mascot mood-${mood}` });
  const svg = mascot(mood, size);
  svg.enter = async (morphed) => {
    if (!morphed) await react(svg, "pop");
    else await new Promise((r) => setTimeout(r, 560)); // it flew in from the last screen: let it settle
    if (mood === "happy") await celebrate(svg);
    if (then) react(svg, then);
  };
  wrap.append(svg);
  return wrap;
}

function header(hello) {
  const row = h("div", { class: "instance" });
  row.append(mascot(hello.state === "unlocked" ? "idle" : "locked", 22), `Rubi · ${hello.fingerprint}`);
  const btn = updatesButton();
  if (btn) row.append(btn);
  return row;
}

// pane groups a block of a wide screen (side by side on large screens, stacked on phones).
function pane(...children) {
  return h("section", { class: "pane" }, ...children);
}

// ---------- updates button (on every screen) ----------

let activeCtx = null; // the page's Rubi connection, once known
let currentView = null; // re-renders the screen the Updates page was opened from
let updatesCache = null; // {at, items}

function outdated(items) {
  return (items || []).filter((x) => x.available).length;
}

async function loadUpdates(force = false) {
  if (!activeCtx || activeCtx.hello.state !== "unlocked") return null;
  if (!force && updatesCache && Date.now() - updatesCache.at < 60000) return updatesCache.items;
  const res = await activeCtx.client.call("updates.list");
  updatesCache = { at: Date.now(), items: res.updates };
  return res.updates;
}


// openSettings goes to the settings from any screen. A link made for something else (an approval, an
// unlock) first asks Rubi for a settings ticket; changes there still need the passkey or password.
async function openSettings(ctx) {
  if (ctx.demo || ctx.link.a === "settings") return whenUnlocked(ctx, () => settingsScreen(ctx));
  let res;
  try {
    res = await ctx.client.call("session.settings");
  } catch (e) {
    return fatal(/unknown operation/.test(e.message)
      ? "This Rubi is older and can't open its settings from here. Ask your agent for a settings link."
      : (e instanceof UserError ? e.message : friendly(e)));
  }
  const link = { ...ctx.link, a: "settings", t: res.ticket };
  const sctx = { ...ctx, link, client: tracked(new RubiClient(link)) };
  activeCtx = sctx;
  whenUnlocked(sctx, () => settingsScreen(sctx));
}

let onSettings = false; // the settings screen doesn't need a button to itself

// updatesButton is the corner button on every screen: it opens the settings, and shows how many
// updates are waiting (they're in the settings, under Updates).
function updatesButton() {
  if (!activeCtx || activeCtx.hello.state === "unpaired" || onSettings) return null;
  const badge = h("span", { class: "badge", hidden: true });
  const btn = h("button", { class: "updates-btn settings-btn", type: "button", title: "Settings", "aria-label": "Settings" },
    icon("gear", 18), badge);
  btn.onclick = () => busy(btn, () => openSettings(activeCtx));
  loadUpdates().then((items) => {
    const n = outdated(items);
    if (n) {
      badge.textContent = String(n);
      badge.hidden = false;
      btn.classList.add("has-updates");
      btn.title = `Settings · ${n === 1 ? "1 update" : n + " updates"} available`;
      btn.setAttribute("aria-label", btn.title);
    }
  }).catch(() => {});
  return btn;
}

async function updatesScreen(ctx) {
  if (ctx.hello.state !== "unlocked") return whenUnlocked(ctx, () => updatesScreen(ctx));
  const back = currentView || (() => statusScreen(ctx, ""));
  const err = errorBox();
  let items;
  try {
    items = await loadUpdates(true);
  } catch (e) {
    return fatal(e instanceof UserError ? e.message : friendly(e));
  }
  const again = () => updatesScreen(ctx);
  const sorted = [...items].sort((a, b) => Number(!!b.available) - Number(!!a.available));
  const rows = sorted.map((it) => {
    const notify = h("input", { type: "checkbox", role: "switch", class: "switch sm", checked: it.notify, "aria-label": `Tell my agent about new versions of ${it.name}` });
    const saved = h("span", { class: "muted small saved" });
    notify.addEventListener("change", async () => {
      saved.textContent = "Saving…";
      try {
        await ctx.client.call("updates.notify", { id: it.id, notify: notify.checked });
        saved.textContent = "Saved";
        setTimeout(() => { saved.textContent = ""; }, 1400);
      } catch (x) {
        notify.checked = !notify.checked;
        saved.textContent = "";
        showError(err, x);
      }
    });
    const update = it.available ? h("button", { class: "primary small", onclick: async (e) => {
      await busy(e.currentTarget, async () => {
        const op = it.id === "rubi" ? "updates.rubi" : "updates.plugin";
        confirmChange(ctx, await ctx.client.call(op, { id: it.id }), again, "Back to updates");
      }).catch((x) => showError(err, x));
    } }, icon("download", 16), "Update") : null;
    return h("article", { class: "urow" + (it.available ? " outdated" : "") },
      it.id === "rubi" ? h("span", { class: "urow-mascot" }, mascot("idle", 36)) : badge(it.name, 36),
      h("div", { class: "urow-text" }, h("strong", {}, it.name),
        h("span", { class: "muted small" }, it.available ? h("span", {}, it.current, " ", icon("chevron", 12), " ", h("b", { class: "ok" }, it.latest)) : `${it.current} · up to date`)),
      update,
      h("label", { class: "urow-notify", title: "Tell my agent when a new version is out" }, h("span", { class: "muted small" }, "Tell agent"), notify, saved));
  });
  const n = outdated(items);
  screen(
    { cls: "wide", focus: false },
    header(ctx.hello),
    backBar("Settings", () => openSettings(ctx)),
    h("h1", {}, "Updates"),
    h("p", {}, n ? `${n === 1 ? "1 update is" : n + " updates are"} available.` : "Everything is up to date."),
    err,
    h("div", { class: "ulist" }, rows),
    h("p", { class: "muted small" }, "Rubi learns about new versions within seconds. Turn off “Tell agent” for anything you'd rather update from here, without your agent bringing it up. Every update still needs your passkey or password."),
  );
}

function showPanel() {
  document.getElementById("landing").hidden = true;
  root.hidden = false;
}

function showLanding() {
  root.hidden = true;
  document.getElementById("landing").hidden = false;
  enhanceLanding();
  const btn = document.getElementById("copy-install");
  const cmd = document.getElementById("install-cmd");
  if (btn && cmd && navigator.clipboard) {
    btn.hidden = false;
    btn.onclick = async () => {
      try {
        await navigator.clipboard.writeText(cmd.textContent);
        btn.textContent = "Copied";
        setTimeout(() => { btn.textContent = "Copy"; }, 1500);
      } catch {
        /* clipboard blocked: the text is still selectable */
      }
    };
  }
}

function errorBox() {
  return h("p", { class: "error", role: "alert", hidden: true });
}

function showError(box, err) {
  box.textContent = err instanceof UserError ? err.message : friendly(err);
  box.hidden = false;
}

function friendly(err) {
  if (err?.name === "NotAllowedError") return "Cancelled, or the passkey wasn't available. Try again.";
  if (err?.name === "OperationError") return "Wrong password, or the key didn't match.";
  return err?.message || String(err);
}

async function busy(button, fn) {
  for (const e of root.querySelectorAll(".error[role=alert]")) e.hidden = true;
  // The label stays (so the button keeps its size); CSS covers it with three moving dots.
  const label = [...button.childNodes];
  button.disabled = true;
  button.classList.add("is-busy");
  button.setAttribute("aria-busy", "true");
  // If it takes a while, Rubi starts thinking.
  const face = root.querySelector(".screen-mascot .mascot, .req-top .mascot");
  const mood = face?.dataset.mood;
  const slow = setTimeout(() => face && setMood(face, "thinking"), 1500);
  try {
    return await fn();
  } finally {
    clearTimeout(slow);
    if (face?.isConnected && face.dataset.mood === "thinking") setMood(face, mood);
    button.disabled = false;
    button.classList.remove("is-busy");
    button.removeAttribute("aria-busy");
    if (button.isConnected && !button.classList.contains("is-done")) button.replaceChildren(...label);
  }
}

function fmtTime(iso) {
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

// ---------- pinning: remember each instance's key and the newest vault version seen ----------

const STORE = "rubi.instances.v1";

function loadPins() {
  try {
    return JSON.parse(localStorage.getItem(STORE) || "{}");
  } catch {
    return {};
  }
}

function savePin(instance, k, version) {
  try {
    const pins = loadPins();
    const prev = pins[instance] || {};
    pins[instance] = { k, maxVersion: Math.max(prev.maxVersion || 0, version || 0), seenAt: new Date().toISOString() };
    localStorage.setItem(STORE, JSON.stringify(pins));
  } catch {
    /* private mode: pinning just isn't remembered */
  }
}

// ---------- landing: the live mascot and the approval story ----------

let landingDone = false;
function enhanceLanding() {
  if (landingDone) return;
  landingDone = true;
  document.documentElement.classList.add("js");

  // Rubi, alive, watching the pointer. Clicking it makes it smile and shine.
  const img = document.getElementById("hero-mascot");
  if (img) {
    const svg = mascot("idle", 240, { follow: true });
    svg.classList.add("hero-mascot");
    svg.addEventListener("click", async () => {
      setMood(svg, "happy");
      await celebrate(svg);
      setMood(svg, "idle");
    });
    img.replaceWith(svg); // the same shape in the same place, so no entrance: it simply comes alive
    landingMascot = svg;
  }

  // The headline arrives word by word.
  const title = document.getElementById("hero-title");
  if (title && !reducedMotion()) {
    const words = title.textContent.split(" ");
    title.replaceChildren(...words.flatMap((w, i) => [h("span", { class: "word", style: `--w:${i}` }, w), " "]));
  }

  for (const f of document.querySelectorAll(".feature[data-icon]")) f.prepend(h("span", { class: "feature-icon" }, icon(f.dataset.icon, 22)));

  // A small story of what Rubi does, on a loop: the agent asks, you approve, it's done.
  const story = document.getElementById("story");
  if (story) startStory(story);

  // Blocks below the fold rise as they scroll into view.
  if ("IntersectionObserver" in window && !reducedMotion()) {
    const io = new IntersectionObserver((entries) => {
      for (const e of entries) if (e.isIntersecting) { e.target.classList.add("seen"); e.target.closest(".steps")?.classList.add("seen"); io.unobserve(e.target); }
    }, { threshold: 0.15 });
    document.querySelectorAll(".steps li, .feature, .integrations .chip, .closer").forEach((el, i) => {
      el.classList.add("reveal");
      el.style.setProperty("--d", `${(i % 4) * 70}ms`);
      io.observe(el);
    });
  }
}
let landingMascot = null;

const STORY = [
  { who: "agent", text: "Send Anna the dinner invite?", sub: "iCloud Mail · Send email" },
  { who: "you", text: "Approve with Passkey", sub: "Face ID" },
  { who: "rubi", text: "Sent", sub: "Rubi will tell your agent when Anna replies" },
];

function startStory(story) {
  const card = h("div", { class: "story-card" });
  story.append(card);
  let i = 0;
  const show = () => {
    const step = STORY[i];
    const next = h("div", { class: `story-step who-${step.who}` },
      h("span", { class: "story-icon" }, icon(step.who === "agent" ? "bot" : step.who === "you" ? "passkey" : "check", 18)),
      h("div", {}, h("strong", {}, step.text), h("small", {}, step.sub)));
    card.replaceChildren(next);
    if (landingMascot) {
      setMood(landingMascot, step.who === "rubi" ? "happy" : step.who === "you" ? "alert" : "idle");
      if (step.who === "rubi") celebrate(landingMascot);
      if (step.who === "agent") react(landingMascot, "nod");
    }
    i = (i + 1) % STORY.length;
  };
  show();
  if (!reducedMotion()) setInterval(() => { if (!document.hidden) show(); }, 2800);
}

// ---------- entry ----------

async function main() {
  if (location.hash.startsWith("#demo")) return demoMain(location.hash.slice(5).replace(/^=/, ""));
  if (!window.isSecureContext) return fatal("This page must be opened over HTTPS.");
  const link = parseLink(location.hash);
  // Keep the one-time codes out of the address bar and history.
  if (location.hash) history.replaceState(null, "", location.pathname + location.search);
  if (!link) return showLanding();
  // Another page could frame the panel under a decoy and get the user to approve something unseen.
  if (window.top !== window.self) return fatal("Open this link directly, not inside another page.", true);
  if (!(await supportsX25519())) {
    return fatal("This browser is too old for Rubi's encryption. Update it (Safari 17+, Chrome 133+, Firefox 130+).");
  }

  connecting();
  const client = tracked(new RubiClient(link));
  let hello;
  try {
    hello = await client.call("hello");
  } catch (e) {
    return fatal(e instanceof UserError ? e.message : friendly(e));
  }

  // What the user sees and compares is computed here from the link's key, never taken from the server.
  const fp = await fingerprint(link.k);
  if (hello.fingerprint !== fp) return fatal("This Rubi didn't prove who it is. Don't continue; tell your agent.", true);
  const pins = loadPins();
  const pin = pins[hello.instance];
  const sameKeyElsewhere = Object.entries(pins).some(([id, p]) => p.k === link.k && id !== hello.instance);
  if (pin && pin.k !== link.k || sameKeyElsewhere) {
    return fatal("This link claims to be a Rubi you've used before, but its key is different. " +
      "Someone may be impersonating it. Don't continue; tell your agent.", true);
  }
  const ctx = { client, link, hello, pin };
  activeCtx = ctx;

  if (link.a === "pair") return pairScreen(ctx);
  if (hello.state === "unpaired") return fatal("This Rubi hasn't been set up yet. Ask your agent for a setup link.");
  if (link.a.startsWith("approve:")) return approveScreen(ctx, link.a.slice("approve:".length));
  if (link.a === "unlock") return unlockScreen(ctx);
  if (link.a.startsWith("setup:")) return whenUnlocked(ctx, () => setupScreen(ctx, link.a.slice("setup:".length)));
  if (link.a === "settings") return whenUnlocked(ctx, () => settingsScreen(ctx));
  if (link.a.startsWith("agent:")) return whenUnlocked(ctx, () => agentScreen(ctx, link.a.slice("agent:".length)));
  return statusScreen(ctx, "");
}

// ---------- demo: every screen with made-up data (see demo.js) ----------

function demoMain(which) {
  const ctx = { client: tracked(new DemoClient()), link: { a: "demo", k: "demo" }, hello: { ...DEMO_HELLO }, demo: true };
  activeCtx = ctx;
  const go = (name) => {
    history.replaceState(null, "", "#demo=" + name);
    demoScreen(ctx, name);
  };
  ctx.gallery = () => {
    history.replaceState(null, "", "#demo");
    screen(
      { cls: "wide" },
      face("happy", 88),
      h("p", { class: "eyebrow center" }, "Demo"),
      h("h1", { class: "center" }, "Try the Rubi panel"),
      h("p", { class: "center muted" }, "Everything here is made up: no Rubi, no keys, nothing leaves this page. Approve, decline, poke around."),
      h("div", { class: "demo-grid" }, DEMO_SCREENS.map(([id, label]) =>
        h("button", { class: "demo-tile", type: "button", onclick: () => go(id) }, label))),
    );
  };
  if (!which) return ctx.gallery();
  demoScreen(ctx, which);
}

function demoScreen(ctx, which) {
  const back = () => ctx.gallery();
  ctx.hello = { ...DEMO_HELLO };
  switch (which) {
    case "pair":
      ctx.hello = { ...DEMO_HELLO, state: "unpaired" };
      return pairScreen(ctx);
    case "unlock":
      ctx.hello = { ...DEMO_HELLO, state: "locked" };
      return unlockFlow(ctx, "Unlock Rubi", "Rubi restarted and locked itself. Unlock it to let your agent continue.",
        () => { ctx.hello = { ...DEMO_HELLO }; statusScreen(ctx, "Unlocked. You can go back to your agent."); });
    case "send": case "install": case "reveal": case "door":
      return approveScreen(ctx, which, { onDone: back, doneLabel: "Back to the demo" });
    case "settings":
      return settingsScreen(ctx, "plugins");
    case "approvals":
      return settingsScreen(ctx, "approvals");
    case "security":
      return settingsScreen(ctx, "security");
    case "store":
      return storeScreen(ctx, back);
    case "config":
      return pluginConfigScreen(ctx, "icloud-mail", { done: back });
    case "setup":
      return setupScreen(ctx, "telegram", back);
    case "home":
      return homeDeviceScreen(ctx, back);
    case "bots":
      return grokBotScreen(ctx);
    case "updates":
      return updatesScreen(ctx);
    case "status":
      return statusScreen(ctx, "Unlocked. You can go back to your agent.");
    case "sent": case "declined": case "failed": {
      const a = { kind: "icloud-mail.send", summary: "Send email to Anna Petrova", state: { sent: "executed", declined: "denied", failed: "failed" }[which],
        result: { tracking: {} }, error: which === "failed" ? "iCloud rejected the login. Check the app-specific password in Rubi's settings." : undefined };
      return resultScreen(ctx, a, null, { preview: { to: "Anna Petrova <anna@example.com>", subject: "Dinner on Friday", body: "" }, notified: true,
        option: { label: "Send and notify on reply" } });
    }
    case "error":
      return fatal("Can't reach your Rubi. It may be restarting; wait a moment, or ask your agent for a new link.");
  }
  return ctx.gallery();
}

function fatal(message, danger = false) {
  const ub = danger ? null : updatesButton(); // never next to an impersonation warning
  screen(ub ? h("div", { class: "top-actions" }, ub) : null, danger ? null : face("concern", 76, "shake"), h("h1", {}, danger ? "Stop" : "Something's wrong"),
    h("p", { class: danger ? "error" : "" }, message));
}

// ---------- pairing ----------

function pairScreen(ctx) {
  const { hello } = ctx;
  if (hello.state !== "unpaired") {
    return fatal("This Rubi is already set up. Ask your agent for an unlock link instead.");
  }
  const err = errorBox();
  const state = { wraps: [], approvers: [], passkeyCanUnlock: false, credentialId: "" };
  const dek = rand(32);

  async function addPasskey(btn) {
    if (ctx.demo) {
      state.passkeyCanUnlock = true;
      return passwordStep();
    }
    await busy(btn, async () => {
      const prfSalt = rand(32);
      const pk = await createPasskey({ instance: hello.instance, fingerprint: hello.fingerprint, prfSalt });
      state.approvers.push(pk.approver);
      state.credentialId = pk.credentialId;
      if (pk.prfOutput) {
        const id = "w_" + b64u.enc(rand(8));
        const kek = await prfKek(pk.prfOutput, prfSalt);
        state.wraps.push({ id, kind: "passkey", credential_id: pk.credentialId, prf_salt: b64u.enc(prfSalt),
          label: "Passkey", ...(await wrapDek(kek, dek, hello.instance, id)) });
        state.passkeyCanUnlock = true;
      }
      passwordStep();
    }).catch((e) => showError(err, e));
  }

  function passwordStep() {
    const err2 = errorBox();
    const pw = h("input", { type: "password", autocomplete: "new-password", minlength: "12", placeholder: "Password (12+ characters)", required: true });
    const pw2 = h("input", { type: "password", autocomplete: "new-password", placeholder: "Repeat password", required: true });
    const user = h("input", { type: "text", autocomplete: "username", value: `Rubi ${hello.fingerprint}`, hidden: true, readonly: true });
    const go = h("button", { class: "primary", type: "submit" }, state.passkeyCanUnlock ? "Save backup password" : "Set password");
    const meter = h("div", { class: "meter", "data-s": "0", "aria-hidden": "true" }, h("i"), h("i"), h("i"), h("i"));
    const meterText = h("small", { class: "muted meter-text", "aria-live": "polite" }, "");
    pw.addEventListener("input", () => {
      const sc = strength(pw.value);
      meter.dataset.s = String(sc);
      meterText.textContent = pw.value ? ["Too short", "Weak", "Fair", "Good", "Strong"][sc] : "";
    });
    const form = h("form", {
      onsubmit: (e) => {
        e.preventDefault();
        if (pw.value.length < 12) return showError(err2, new UserError("Use at least 12 characters."));
        if (pw.value !== pw2.value) return showError(err2, new UserError("The passwords don't match."));
        busy(go, () => finish(pw.value)).catch((x) => showError(err2, x));
      },
    }, user, pwField(pw), h("div", { class: "meter-row" }, meter, meterText), pwField(pw2), go);
    screen(
      header(hello),
      stepper(SETUP_STEPS, 1),
      h("h1", {}, state.passkeyCanUnlock ? "Add a backup password" : "Choose a password"),
      state.passkeyCanUnlock
        ? h("p", {}, "Recommended: if you ever lose your passkey, this is the only other way back in. Save it in your Passwords app.")
        : h("p", {}, state.approvers.length
          ? "Your passkey can approve actions, but this device can't use it to unlock Rubi. A password is required."
          : "Rubi will be locked with this password. Save it in your Passwords app: nobody can recover it."),
      form, err2,
      state.passkeyCanUnlock
        ? h("button", { class: "link", onclick: (e) => busy(e.target, () => finish(null)).catch((x) => showError(err2, x)) }, "Skip")
        : null,
    );
  }

  async function finish(password) {
    if (ctx.demo) return connectAgentScreen({ ...ctx, hello: { ...DEMO_HELLO } });
    const args = { code: ctx.link.p, dek: b64u.enc(dek), wraps: [...state.wraps], approvers: state.approvers,
      method: state.passkeyCanUnlock ? "passkey" : "password", credential_id: state.credentialId || undefined };
    if (password) {
      const kdf = newPasswordKdf();
      const kek = await passwordKek(password, kdf);
      const id = "w_" + b64u.enc(rand(8));
      args.wraps.push({ id, kind: "password", kdf, label: "Password", ...(await wrapDek(kek, dek, hello.instance, id)) });
      args.password_approve_key = b64u.enc(await approveKey(kek));
    }
    if (!args.wraps.length) throw new UserError("Add a password: this passkey can't unlock Rubi on its own.");
    const res = await ctx.client.call("pair", args);
    savePin(hello.instance, ctx.link.k, res.vault_version);
    const settingsCtx = { ...ctx, client: tracked(new RubiClient({ ...ctx.link, t: res.ticket })) };
    settingsCtx.hello = await settingsCtx.client.call("hello");
    connectAgentScreen(settingsCtx);
  }

  const option = (ic, title, text, tag, onclick) => h("button", { type: "button", class: "option-card", onclick },
    h("span", { class: "option-icon" }, icon(ic, 22)),
    h("span", { class: "option-text" }, h("strong", {}, title, tag ? h("span", { class: "tag" }, tag) : null), h("small", {}, text)),
    icon("chevron", 18, "option-go"));
  screen(
    header(hello),
    face("idle", 96),
    stepper(SETUP_STEPS, 0),
    h("h1", {}, "Set up Rubi"),
    h("p", {}, "Rubi will be locked with a key only you hold. Choose how you'll unlock it and approve your agent's actions."),
    h("div", { class: "options" },
      passkeysAvailable() ? option("passkey", "Use a passkey", "Face ID, Touch ID or your phone. Nothing to remember.", "Recommended", (e) => addPasskey(e.currentTarget)) : null,
      option("key", "Use a password only", "At least 12 characters. Save it in your Passwords app.", null, passwordStep)),
    err,
    h("p", { class: "muted small center" }, `Instance ${hello.instance}`),
  );
}

// parseWebhook pulls the routine's webhook URL and key out of whatever the user pasted: the "POST to" and
// "key" fields, the "header" field, or all of them at once, in any order.
function parseWebhook(text) {
  const t = String(text || "");
  const url = (t.match(/https?:\/\/[^\s"'<>]+/) || [])[0] || "";
  let key = (t.match(/Bearer\s+([^\s"']+)/i) || [])[1] || "";
  if (!key) {
    key = t.replace(url, " ").split(/\s+/).map((w) => w.replace(/^(authorization|key|post|to):?$/i, ""))
      .filter((w) => w.length >= 16 && !/^https?:/i.test(w))[0] || "";
  }
  return { url, key };
}

// agentForm asks for an agent's routine webhook in one paste box. fixedName locks the name (agent links).
function agentForm(ctx, { fixedName, onSaved, err }) {
  const name = h("input", { type: "text", placeholder: "The Grok Bot's name*", autocomplete: "off", value: fixedName || "", required: true });
  if (fixedName) name.readOnly = true;
  const paste = h("textarea", { rows: "4", placeholder: "Paste here: the \u201cPOST to\u201d address and the \u201cheader\u201d (or \u201ckey\u201d)", autocapitalize: "none", spellcheck: "false", required: true });
  const parsed = h("p", { class: "muted small" });
  const show = () => {
    const w = parseWebhook(paste.value);
    parsed.textContent = !paste.value.trim() ? ""
      : `${w.url ? "Address: " + w.url.replace(/^(https?:\/\/[^/]+).*$/, "$1/\u2026") : "Address: not found yet"} \u00b7 ${w.key ? "Key: \u2022\u2022\u2022\u2022" + w.key.slice(-4) : "Key: not found yet"}`;
  };
  paste.addEventListener("input", show);
  // What this Bot will hear about, chosen before the connection is confirmed (part of the same approval).
  const boxes = [];
  const subsBox = h("fieldset", { class: "field", hidden: true }, h("legend", {}, "This Bot gets notified about"),
    h("small", {}, "Results of what this Bot asks for always reach it. Notifications no Bot chose go to the default Bot."));
  const subsList = h("div", { class: "checks" });
  subsBox.insertBefore(subsList, subsBox.lastChild);
  ctx.client.call("agents.get").then((data) => {
    const existing = (data.agents || []).find((a) => fixedName && a.name.toLowerCase() === fixedName.toLowerCase());
    const first = !(data.agents || []).length;
    for (const src of data.sources || []) {
      const checked = existing ? (existing.subscriptions || []).includes(src.id) : first;
      const b = h("input", { type: "checkbox", value: src.id, checked });
      boxes.push(b);
      subsList.append(h("label", { class: "check" }, b, h("span", {}, src.name)));
    }
    subsBox.hidden = boxes.length === 0;
  }).catch(() => {});
  const go = h("button", { class: "primary", type: "submit" }, "Connect");
  const form = h("form", {
    onsubmit: (e) => {
      e.preventDefault();
      const w = parseWebhook(paste.value);
      if (!w.url) return showError(err, new UserError("Paste the webhook address (\u201cPOST to\u201d) as well."));
      if (!w.key) return showError(err, new UserError("Paste the webhook key or header as well."));
      busy(go, async () => {
        const args = { name: name.value.trim(), url: w.url, key: w.key };
        if (boxes.length) args.sources = boxes.filter((b) => b.checked).map((b) => b.value);
        const res = await ctx.client.call("agent.add", args);
        paste.value = "";
        confirmChange(ctx, res, () => onSaved(name.value.trim()), "Finish");
      }).catch((x) => showError(err, x));
    },
  }, fixedName ? null : h("label", { class: "field" }, h("span", {}, "Name"), name),
  h("label", { class: "field" }, h("span", {}, "Webhook"), paste), parsed, subsBox, go);
  return form;
}

function agentHowTo(name) {
  return h("ol", { class: "steps-list" },
    h("li", {}, "Open the Grok Bot app on your computer (these values are only shown there)."),
    h("li", {}, `Open ${name ? name + "\u2019s" : "your Bot\u2019s"} routine \u201cRubi events\u201d. Its Webhook section shows \u201cPOST to\u201d, \u201ckey\u201d and \u201cheader\u201d.`),
    h("li", {}, "Copy \u201cPOST to\u201d and \u201cheader\u201d and paste both into the box below."));
}

async function agentConnected(ctx, name) {
  try {
    await ctx.client.call("webhook.test", { name });
  } catch (_) { /* the test event is a courtesy */ }
  screen(
    header(ctx.hello),
    face("happy", 96),
    h("h1", {}, `${name} is connected`),
    h("p", {}, "Rubi just sent it a test event; it will confirm in the chat. From now on it continues on its own after you approve something, and hears about replies right away."),
    h("p", { class: "muted" }, "You can close this page."),
  );
}

// agentScreen is opened from rubi_link("agent:<name>"), ideally in the Grok Bot desktop app.
function agentScreen(ctx, name) {
  const err = errorBox();
  screen(
    face("idle", 96),
    header(ctx.hello),
    h("h1", {}, `Connect ${name}`),
    h("p", {}, "So Rubi can tell this Bot when you approve something or a reply arrives. The routine runs only when something happens, never on a schedule."),
    agentHowTo(name),
    agentForm(ctx, { fixedName: name, err, onSaved: (n) => agentConnected(ctx, n) }),
    err,
  );
}

// connectAgentScreen is the last step after pairing. The webhook values are only visible in the Grok Bot
// desktop app, so on a phone the usual path is the link the agent sends.
function connectAgentScreen(ctx) {
  const err = errorBox();
  const here = h("details", { class: "sideload" },
    h("summary", {}, "I have the routine open here"),
    agentHowTo(""),
    agentForm(ctx, { err, onSaved: (n) => agentConnected(ctx, n) }));
  screen(
    header(ctx.hello),
    face("happy", 96),
    stepper(SETUP_STEPS, 2),
    h("h1", {}, "Connect your agent"),
    h("p", {}, "Rubi is set up and unlocked. One more thing: let Rubi wake your agent when you approve something or a reply arrives."),
    h("p", {}, "Your agent sent you a link called \u201cConnect\u201d. Open it in the Grok Bot app on your computer, next to its \u201cRubi events\u201d routine: the routine\u2019s webhook is only shown there."),
    here,
    err,
  );
}

// ---------- unlocking ----------

// unlockKeys fetches the wrapped keys. They must belong to the instance this page verified: an impostor
// holding a leaked copy of the real wraps (from a backup) would otherwise get the user to unwrap the
// real key and hand it over.
async function unlockKeys(ctx) {
  const keys = await ctx.client.call("unlock.keys");
  if (keys.instance !== ctx.hello.instance) {
    fatal("This Rubi didn't prove who it is. Don't continue; tell your agent.", true);
    throw new UserError("This Rubi didn't prove who it is.");
  }
  return keys;
}

// unlockFlow renders the unlock UI and calls onDone() once Rubi is unlocked.
async function unlockFlow(ctx, title, intro, onDone) {
  const { client, hello } = ctx;
  const keys = await unlockKeys(ctx);
  const err = errorBox();
  const passkeyWraps = keys.wraps.filter((w) => w.kind === "passkey");
  const passwordWraps = keys.wraps.filter((w) => w.kind === "password");

  async function send(dek, method, credentialId) {
    let res;
    try {
      res = await client.call("unlock", { dek: b64u.enc(dek), min_vault_version: ctx.pin?.maxVersion || 0, method,
        credential_id: credentialId });
    } finally {
      dek.fill(0);
      pw.value = "";
    }
    savePin(hello.instance, ctx.link.k, res.vault_version);
    await onDone();
  }

  async function withPasskey(btn) {
    await busy(btn, async () => {
      if (ctx.demo) return onDone();
      const r = await evalPrf(passkeyWraps.map((w) => ({ credentialId: w.credential_id, prfSalt: b64u.dec(w.prf_salt) })));
      const w = passkeyWraps.find((x) => x.credential_id === r.credentialId);
      if (!w) throw new UserError("That passkey isn't registered with this Rubi.");
      const dek = await unwrapDek(await prfKek(r.prfOutput, b64u.dec(w.prf_salt)), w, keys.instance);
      await send(dek, "passkey", w.credential_id);
    }).catch((e) => showError(err, e));
  }

  const pw = h("input", { type: "password", autocomplete: "current-password", placeholder: "Password" });
  const user = h("input", { type: "text", autocomplete: "username", value: `Rubi ${hello.fingerprint}`, hidden: true, readonly: true });
  const go = h("button", { class: passkeyWraps.length ? "secondary" : "primary", type: "submit" }, "Unlock with password");
  const form = passwordWraps.length ? h("form", {
    onsubmit: (e) => {
      e.preventDefault();
      busy(go, async () => {
        if (ctx.demo) return onDone();
        for (const w of passwordWraps) {
          const kek = await passwordKek(pw.value, w.kdf);
          try {
            return await send(await unwrapDek(kek, w, keys.instance), "password");
          } catch (x) {
            if (x?.name !== "OperationError") throw x;
          }
        }
        throw new UserError("Wrong password.");
      }).catch((x) => showError(err, x));
    },
  }, user, pwField(pw), go) : null;

  screen(
    header(hello),
    face("locked", 96),
    h("h1", {}, title),
    h("p", {}, intro),
    passkeyWraps.length && passkeysAvailable()
      ? h("button", { class: "primary big", onclick: (e) => withPasskey(e.currentTarget) }, icon("passkey", 20), "Unlock with Passkey")
      : null,
    form,
    err,
  );
}

async function unlockScreen(ctx) {
  if (ctx.hello.state === "unlocked") return statusScreen(ctx, "Rubi is already unlocked.");
  try {
    await unlockFlow(ctx, "Unlock Rubi", "Rubi restarted and locked itself. Unlock it to let your agent continue.",
      () => statusScreen(ctx, "Unlocked. You can go back to your agent."));
  } catch (e) {
    fatal(e instanceof UserError ? e.message : friendly(e));
  }
}

async function statusScreen(ctx, message) {
  currentView = () => statusScreen(ctx, message);
  let st = null;
  try {
    st = await ctx.client.call("status");
  } catch {
    /* status is informational */
  }
  const receipts = (st?.receipts || []).slice(-5).reverse();
  screen(
    header(ctx.hello),
    face(st?.state === "unlocked" ? "happy" : "locked"),
    h("h1", { class: "center" }, st?.state === "unlocked" ? "Rubi is unlocked" : "Rubi"),
    message ? h("p", { class: "center muted" }, message) : null,
    receipts.length ? h("h2", {}, "Recent unlocks") : null,
    receipts.length ? h("ul", { class: "acct-list" }, receipts.map((r) => h("li", {},
      h("span", { class: "avatar-dot small" }, icon(r.method === "passkey" ? "passkey" : "key", 14)),
      h("span", { class: "acct-name" }, `${r.event} with ${r.method}`, h("small", { class: "muted" }, fmtTime(r.at)))))) : null,
    receipts.length ? h("p", { class: "muted" }, "Don't recognize one of these? Lock Rubi and tell your agent.") : null,
    integrityLine(st),
    st?.state === "unlocked"
      ? h("button", { class: "secondary", onclick: async (e) => {
        await busy(e.currentTarget, () => ctx.client.call("lock"));
        statusScreen(ctx, "Rubi is locked.");
      } }, icon("lock", 18), "Lock Rubi now")
      : null,
  );
}

// ---------- approvals ----------

const FIELD_LABELS = {
  from: "From", to: "To", cc: "Cc", bcc: "Bcc", subject: "Subject", in_reply_to: "In reply to", body: "Message",
  plugin: "Plugin", about: "About", warning: "Warning", review: "Review", publisher: "Publisher",
  website: "Publisher website", source: "Source",
  integration: "Integration", account: "Account", agent: "Bot", notifies_about: "Notifies about", current: "Current version", new_version: "New version",
  new_permissions: "New permissions", back_to: "Back to", will_ask_for: "Will ask you for", can: "Can", connects_to: "Connects to",
  can_notify_about: "Can notify your agent about", effect: "Effect", webhook: "Webhook",
  release_notes: "Release notes", verification: "Verification",
};

// An icon for each well-known preview field.
const FIELD_ICONS = {
  from: "mail", to: "mail", subject: "mail", date: "clock", hidden_because: "eye", plugin: "plug", about: "sparkle", publisher: "user", website: "link",
  source: "link", review: "shield", warning: "warn", new_permissions: "warn", can: "check", connects_to: "globe",
  will_ask_for: "key", can_notify_about: "bell", notifies_about: "bell", rubi_home: "home", account: "user",
  integration: "plug", agent: "bot", webhook: "link", effect: "sparkle", reason: "eye", current: "clock",
  new_version: "download", back_to: "undo", shortcut: "sparkle", folder: "folder", emails: "mail", input: "sparkle",
  allows: "shield", fingerprint: "key", web_addresses: "link", release_notes: "sparkle", verification: "shield",
};

// Preview fields that need the user's attention.
const ATTENTION = new Set(["warning", "new_permissions"]);

// splitAddresses turns `Anna <a@x>, b@y` into [{name, email}] for display.
function splitAddresses(list) {
  return (String(list || "").match(/[^,<]*<[^>]+>|[^,]+/g) || []).map((part) => {
    const m = part.trim().match(/^"?([^"<]*?)"?\s*<([^>]+)>$/);
    return m ? { name: m[1].trim(), email: m[2].trim() } : { name: "", email: part.trim() };
  }).filter((a) => a.email);
}

function recipients(label, list, note) {
  const addrs = splitAddresses(list);
  if (!addrs.length) return null;
  return h("div", { class: "mail-row" },
    h("span", { class: "mail-label" }, label),
    h("div", { class: "chips-col" }, addrs.map((a) => h("span", { class: "person" },
      h("span", { class: "avatar-dot" }, (a.name || a.email).trim().charAt(0).toUpperCase()),
      h("span", { class: "person-text" }, h("span", { class: "person-name" }, a.name || a.email),
        a.name ? h("span", { class: "person-email" }, a.email) : null))),
    note ? h("span", { class: "mail-note" }, note) : null));
}

// mailCard renders an email preview the way a mail app would. Everything is text, never HTML.
function mailCard(p) {
  const body = h("pre", { class: "mail-body clamped" }, p.body || "");
  const more = h("button", { class: "link more", type: "button", "aria-expanded": "false", onclick: () => {
    const expand = body.classList.contains("clamped");
    // Animate between the clamped height and the full height, then let it size naturally.
    body.style.maxHeight = `${body.getBoundingClientRect().height}px`;
    requestAnimationFrame(() => {
      body.classList.toggle("clamped", !expand);
      body.style.maxHeight = expand ? `${body.scrollHeight}px` : "";
    });
    more.textContent = expand ? "Show less" : "Show full message";
    more.setAttribute("aria-expanded", String(expand));
  } }, "Show full message");
  const card = h("div", { class: "mail" },
    recipients("To", p.to),
    recipients("Cc", p.cc),
    recipients("Bcc", p.bcc, "Hidden from other recipients"),
    h("div", { class: "mail-subject" }, p.subject || "(no subject)"),
    p.in_reply_to ? h("div", { class: "mail-thread" }, "Reply in an existing conversation") : null,
    body,
    more,
    p.from ? h("div", { class: "mail-from" }, `From ${p.from}`) : null);
  // Only offer "Show full message" when the text is actually cut off.
  requestAnimationFrame(() => { if (body.scrollHeight <= body.clientHeight + 2) more.remove(); });
  return card;
}

// fieldList renders any other preview as stacked label/value pairs.
function fieldList(preview) {
  if (!preview || typeof preview !== "object") return h("pre", { class: "mail-body" }, JSON.stringify(preview, null, 2));
  const order = Object.keys(FIELD_LABELS);
  const keys = Object.keys(preview).sort((a, b) =>
    (order.indexOf(a) + 1 || 99) - (order.indexOf(b) + 1 || 99) || a.localeCompare(b));
  return h("dl", { class: "fields" }, keys.filter((k) => preview[k] !== null && preview[k] !== undefined && preview[k] !== "")
    .map((k) => h("div", { class: ATTENTION.has(k) ? "field-item attention" : "field-item" },
      h("dt", {}, icon(FIELD_ICONS[k] || (ATTENTION.has(k) ? "warn" : "chevron"), 15), FIELD_LABELS[k] || k.replace(/_/g, " ")),
      h("dd", {}, typeof preview[k] === "string" ? preview[k] : JSON.stringify(preview[k], null, 2)))));
}

const isMail = (p) => p && typeof p === "object" && "to" in p && "subject" in p && "body" in p;

// countdown shows the time left and calls onExpire once. It stops when its element leaves the page.
function countdown(expiresAt, onExpire) {
  const text = h("span", {});
  const el = h("span", { class: "timer", title: `Expires ${fmtTime(expiresAt)}`, role: "timer" }, h("i", { class: "ring", "aria-hidden": "true" }), text);
  const end = new Date(expiresAt).getTime();
  let total = 0;
  const tick = () => {
    const left = Math.max(0, Math.round((end - Date.now()) / 1000));
    total = total || Math.max(left, 1);
    el.style.setProperty("--left", String(left / total));
    text.textContent = left ? `${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}` : "Expired";
    el.classList.toggle("urgent", left < 60);
    if (!left) {
      clearInterval(timer);
      onExpire();
    } else if (!el.isConnected && started) {
      clearInterval(timer);
    }
    started = true;
  };
  let started = false;
  const timer = setInterval(tick, 1000);
  tick();
  return el;
}

function titleFor(a) {
  if ((a.items || []).length) return a.summary; // a batch: "Show 3 private emails to your agent"
  if (a.kind.endsWith(".send")) return "Send this email?";
  if (a.kind.endsWith(".draft")) return "Save this draft?";
  if (a.kind === "rubi.update") return "Update Rubi?";
  const name = a.preview?.plugin;
  if (a.kind === "rubi.plugin.install" && name) return `Install ${name}?`;
  if (a.kind === "rubi.plugin.update" && name) return `Update ${name}?`;
  if (a.kind === "rubi.plugin.remove" && name) return `Remove ${name}?`;
  if (a.kind === "rubi.plugin.rollback" && name) return `Roll ${name} back to ${a.preview.back_to}?`;
  return a.summary;
}

// approveScreen shows one pending approval. opts.onDone (for settings changes) adds a way back.
async function approveScreen(ctx, id, opts = {}) {
  currentView = () => approveScreen(ctx, id, opts);
  if (ctx.hello.state !== "unlocked") {
    try {
      return await unlockFlow(ctx, "Unlock Rubi to review", "Rubi is locked. Unlock it first, then review the request.",
        async () => {
          ctx.hello = await ctx.client.call("hello");
          approveScreen(ctx, id, opts);
        });
    } catch (e) {
      return fatal(e instanceof UserError ? e.message : friendly(e));
    }
  }

  let info;
  try {
    info = await ctx.client.call("approval.get", { approval_id: id });
  } catch (e) {
    return fatal(e instanceof UserError ? e.message : friendly(e));
  }
  const a = info.approval;
  const notified = !!info.agent_notified;
  if (a.state !== "pending") return resultScreen(ctx, a, opts.onDone, { notified, doneLabel: opts.doneLabel });

  const err = errorBox();
  const approverIds = (info.approvers || []).map((x) => x.credential_id);
  const pwInput = h("input", { type: "password", autocomplete: "current-password", placeholder: "Your Rubi password" });
  const pwUser = h("input", { type: "text", autocomplete: "username", value: `Rubi ${ctx.hello.fingerprint}`, hidden: true, readonly: true });
  const pwBox = h("form", { class: "pw-box", onsubmit: (e) => e.preventDefault() }, pwUser, pwInput);
  pwBox.hidden = approverIds.length > 0 && passkeysAvailable();
  const usePasskey = () => approverIds.length > 0 && passkeysAvailable() && pwBox.hidden;

  // Choosing among options: a switch for the common two-option case with a question ("Notify me when they
  // reply?"), otherwise a list. The chosen option is what the user's passkey signs.
  let chosen = a.options[0].key;
  let choiceUI = null;
  if (a.options.length === 2 && a.question) {
    const sw = h("input", { type: "checkbox", role: "switch", class: "switch", onchange: () => {
      chosen = sw.checked ? a.options[1].key : a.options[0].key;
    } });
    if (a.kind === "rubi.plugin.install") { // update notifications are on unless the user turns them off
      sw.checked = true;
      chosen = a.options[1].key;
    }
    const all = [...splitAddresses(a.preview?.to), ...splitAddresses(a.preview?.cc)];
    const label = !a.kind.endsWith(".send") || !all.length ? a.question.replace(/\?$/, "")
      : all.length === 1 ? `Notify me when ${all[0].name || all[0].email} replies` : "Notify me when someone replies";
    sw.setAttribute("aria-label", label);
    sw.addEventListener("change", () => {
      // Rubi nods when you ask to be notified.
      const m = root.querySelector(".req-top .mascot");
      if (m && sw.checked) react(m, "nod");
    });
    choiceUI = h("label", { class: "switch-row" }, h("span", {}, label), sw);
  } else if (a.options.length > 1) {
    choiceUI = h("div", { class: "choices", role: "radiogroup" }, a.options.map((o, i) => {
      const r = h("input", { type: "radio", name: "opt", value: o.key, checked: i === 0, onchange: () => { chosen = o.key; } });
      return h("label", { class: "choice" }, r, h("span", {}, o.label, o.meaning ? h("small", {}, o.meaning) : null));
    }));
  }

  // A batch: every item has its own checkbox; the passkey signs exactly the chosen set.
  let itemsUI = null;
  const itemBoxes = [];
  if ((a.items || []).length) {
    const sync = () => {
      const keys = itemBoxes.filter((x) => x.b.checked).map((x) => x.key);
      chosen = "items:" + keys.join(",");
      primary.disabled = keys.length === 0;
      count.textContent = `${keys.length} of ${itemBoxes.length} selected`;
    };
    const count = h("p", { class: "muted small" });
    itemsUI = h("div", { class: "batch" }, count, a.items.map((it) => {
      const b = h("input", { type: "checkbox", checked: true, onchange: () => sync() });
      itemBoxes.push({ b, key: it.key });
      return h("label", { class: "batch-item" }, b, h("div", {}, h("strong", {}, it.label),
        it.preview && typeof it.preview === "object" ? fieldList(it.preview) : null));
    }));
    queueMicrotask(sync);
  }

  async function challengeFor(option) {
    if (!(a.items || []).length) return info.challenges[option];
    return (await ctx.client.call("approval.challenge", { approval_id: id, option })).challenge;
  }

  async function passwordProof(option) {
    const pw = pwInput.value;
    if (!pw) throw new UserError("Enter your password first.");
    const keys = await unlockKeys(ctx);
    for (const w of keys.wraps.filter((x) => x.kind === "password")) {
      const kek = await passwordKek(pw, w.kdf);
      try {
        await unwrapDek(kek, w, keys.instance); // proves the password before using it
      } catch {
        continue;
      }
      const mac = await hmacSha256(await approveKey(kek), b64u.dec(await challengeFor(option)));
      return { type: "password", mac: b64u.enc(mac) };
    }
    throw new UserError("Wrong password.");
  }

  const verb = a.options[0].label;
  const primary = h("button", { class: "primary big", type: "button" });
  const setPrimary = () => {
    primary.replaceChildren(icon(usePasskey() ? "passkey" : "key", 20), usePasskey() ? `${verb} with Passkey` : `${verb} with password`);
  };
  setPrimary();
  primary.onclick = () => busy(primary, async () => {
    const proof = ctx.demo ? { type: "demo" } : usePasskey()
      ? await signChallenge(b64u.dec(await challengeFor(chosen)), approverIds)
      : await passwordProof(chosen);
    const res = await ctx.client.call("approval.decide", { approval_id: id, option: chosen, approve: true, proof });
    if (res.state === "executed") { // the button becomes a check before Rubi celebrates
      primary.classList.remove("is-busy");
      primary.classList.add("is-done");
      primary.replaceChildren(icon("check", 22), "Done");
      await new Promise((r) => setTimeout(r, reducedMotion() ? 0 : 560));
    }
    resultScreen(ctx, res, opts.onDone, { preview: a.preview, option: a.options.find((o) => o.key === chosen), notified, doneLabel: opts.doneLabel });
  }).catch((e) => { showError(err, e); setPrimary(); });

  const decline = h("button", { class: "link decline", type: "button", onclick: (e) => busy(e.target, async () =>
    resultScreen(ctx, await ctx.client.call("approval.decide", { approval_id: id, approve: false }), opts.onDone, { preview: a.preview, notified, doneLabel: opts.doneLabel }))
    .catch((x) => showError(err, x)) }, opts.declineLabel || "Decline");

  const timer = countdown(a.expires_at, () => {
    primary.disabled = true;
    showError(err, new UserError("This request expired. Ask your agent to try again."));
  });

  const top = h("div", { class: "req-top" });
  top.append(mascot("idle", 30), h("div", { class: "req-who" },
    h("strong", {}, "Rubi"), h("span", {}, `for your agent · ${ctx.hello.fingerprint}`)), timer);
  const ub = updatesButton();
  if (ub) top.append(ub);

  screen(
    { cls: "approve", focus: false },
    top,
    h("div", { class: "req-grid" },
      h("section", { class: "req-main" },
        h("p", { class: "eyebrow" }, opts.eyebrow || "Approval needed"),
        h("h1", {}, titleFor(a)),
        isMail(a.preview) ? mailCard(a.preview) : fieldList(a.preview),
        itemsUI),
      h("aside", { class: "req-side" },
        choiceUI,
        info.password ? pwBox : null,
        err,
        h("p", { class: "fine" }, icon("lock", 14), "Nothing happens until you approve. Your agent can't approve for you."),
        h("div", { class: "actionbar" },
      primary,
      h("div", { class: "actionbar-row" },
        decline,
        !pwBox.hidden || !info.password ? null : h("button", { class: "link", type: "button", onclick: (e) => {
          pwBox.hidden = false;
          e.target.remove();
          setPrimary();
          pwInput.focus();
        } }, "Use password"))))),
  );
}

// resultScreen shows how an approval ended. extra (optional) carries the preview and chosen option.
function resultScreen(ctx, a, onDone, extra = {}) {
  const executedTitle = a.kind?.endsWith(".send") ? "Sent" : a.kind === "rubi.settings" ? "Saved"
    : a.kind === "rubi.update" ? "Updating…" : a.kind === "rubi.plugin.install" ? "Installed"
    : a.kind === "rubi.plugin.update" ? "Updated" : a.kind === "rubi.plugin.remove" ? "Removed"
    : a.kind === "rubi.plugin.rollback" ? "Rolled back" : a.kind?.endsWith(".private") ? "Shown to your agent"
    : a.kind?.endsWith(".folder") || a.kind?.endsWith(".chat_access") ? "Allowed" : a.kind?.endsWith(".draft") ? "Draft saved" : "Done";
  const titles = {
    executed: executedTitle,
    denied: "Declined",
    failed: "It didn't work",
    expired: "This request expired",
    cancelled: "This request was cancelled",
  };
  const moods = { executed: "happy", failed: "concern" };
  const p = extra.preview;
  const to = isMail(p) ? splitAddresses(p.to)[0] : null;
  const line = to ? `To ${to.name || to.email} · ${p.subject || "(no subject)"}` : a.summary;
  const tracking = a.state === "executed" && ((a.result && a.result.tracking) || (extra.option && /reply/i.test(extra.option.label)));
  const sad = a.state === "denied" || a.state === "cancelled" || a.state === "expired";
  const ok = a.state === "executed";
  const mascotBlock = face(moods[a.state] || "idle", 112, sad ? "sigh" : a.state === "failed" ? "shake" : null);
  if (ok && /^rubi\.(update|plugin\.)/.test(a.kind || "")) updatesCache = null; // versions changed
  const ub = updatesButton();

  // A receipt of what happened, and what the agent knows.
  const rows = [h("div", { class: "receipt-row" }, icon(ok ? "check" : sad ? "close" : "warn", 18), h("span", {}, line))];
  if (tracking) rows.push(h("div", { class: "receipt-row" }, icon("bell", 18), h("span", {}, "Rubi will tell your agent when a reply arrives.")));
  if (ok && a.kind === "rubi.update") rows.push(h("div", { class: "receipt-row" }, icon("download", 18), h("span", {}, "Rubi restarts into the new version in a few seconds and stays unlocked.")));
  if (ok && a.result?.next_step && !onDone) rows.push(h("div", { class: "receipt-row" }, icon("link", 18), h("span", {}, "Your agent will send you a link to set it up.")));
  if (!onDone) {
    rows.push(h("div", { class: "receipt-row muted" }, icon("bot", 18), h("span", {}, ok
      ? (extra.notified ? "Your agent has been told and continues on its own." : "Go back to your agent and tell it you approved, so it can continue.")
      : (extra.notified ? "Nothing was done. Your agent has been told." : "Nothing was done."))));
  }
  screen(
    { cls: `result state-${a.state}` },
    ub ? h("div", { class: "top-actions" }, ub) : null,
    mascotBlock,
    h("h1", { class: "center result-title" }, titles[a.state] || a.state),
    h("div", { class: "receipt" }, rows),
    a.error ? h("p", { class: "error", role: "alert" }, a.error) : null,
    onDone
      ? h("button", { class: "primary", onclick: onDone }, extra.doneLabel || "Back to settings")
      : h("p", { class: "muted center" }, "You can close this page."),
  );
}

// ---------- settings & integration setup ----------

async function whenUnlocked(ctx, next) {
  if (ctx.hello.state === "unlocked") return next();
  try {
    await unlockFlow(ctx, "Unlock Rubi", "Rubi is locked. Unlock it first to change its settings.", async () => {
      ctx.hello = await ctx.client.call("hello");
      next();
    });
  } catch (e) {
    fatal(e instanceof UserError ? e.message : friendly(e));
  }
}

// confirmChange takes the {approval_id, ticket} a settings operation returns and asks for the passkey or the
// password, using a client bound to that approval's ticket.
function confirmChange(ctx, res, onDone, doneLabel) {
  const approvalCtx = ctx.demo ? ctx : { ...ctx, client: tracked(new RubiClient({ ...ctx.link, t: res.ticket })) };
  return approveScreen(approvalCtx, res.approval_id, { eyebrow: "Confirm this change", declineLabel: "Cancel", onDone, doneLabel });
}

async function setupScreen(ctx, id, back) {
  const err = errorBox();
  let entry;
  try {
    entry = (await ctx.client.call("integration.catalog")).integrations.find((i) => i.id === id);
  } catch (e) {
    return fatal(e instanceof UserError ? e.message : friendly(e));
  }
  if (!entry) return fatal("This plugin isn't installed. Ask your agent to install it from the store first.");
  const connected = () => screen(header(ctx.hello), h("h1", {}, `${entry.name} is connected`),
    h("p", {}, "You can close this page and go back to your agent."));
  const finish = back || connected;
  let account = "";
  const done = entry.has_config
    ? () => pluginConfigScreen(ctx, id, { intro: true, account, done: finish })
    : finish;

  // One form per setup step. A plugin may ask for more after the first (a login code, say); the fields
  // entered so far go along with each later step.
  const step = (fieldDefs, secretDefs, message, carried) => {
    const inputs = {};
    const fieldEls = fieldDefs.map((f) => {
      let el;
      if (f.type === "choice") {
        el = h("select", { required: f.required }, ...(f.options || []).map((o) => h("option", { value: o.key }, o.label)));
      } else {
        el = h("input", { type: { email: "email", tel: "tel", number: "text" }[f.type] || "text",
          inputmode: f.type === "number" ? "numeric" : null, placeholder: f.placeholder || "",
          autocomplete: f.type === "email" ? "email" : f.type === "number" ? "one-time-code" : "off",
          required: f.required, autocapitalize: "none" });
      }
      inputs["f:" + f.key] = el;
      return h("label", { class: "field" }, h("span", {}, f.label), el, f.help ? h("small", {}, f.help) : null);
    });
    const secretEls = secretDefs.filter((sec) => !sec.internal).map((sec) => {
      // A service's secret, never the Rubi password: keep password managers from filling it in.
      inputs["s:" + sec.key] = h("input", { type: "password", autocomplete: "new-password", "data-1p-ignore": "", "data-lpignore": "true",
        required: !sec.optional, autocapitalize: "none", spellcheck: "false" });
      return h("label", { class: "field" }, h("span", {}, sec.label), inputs["s:" + sec.key],
        sec.help ? h("small", {}, sec.help) : null,
        /^https:\/\//.test(sec.help_url || "") ? h("a", { href: sec.help_url, target: "_blank", rel: "noopener noreferrer", class: "button-link" }, sec.help_link || "Open the account page") : null);
    });
    const go = h("button", { class: "primary", type: "submit" }, message ? "Continue" : `Connect ${entry.name}`);
    const form = h("form", {
      onsubmit: (e) => {
        e.preventDefault();
        busy(go, async () => {
          const fields = { ...carried }, secrets = {};
          for (const [k, el] of Object.entries(inputs)) {
            (k.startsWith("f:") ? fields : secrets)[k.slice(2)] = el.value;
          }
          go.textContent = "Checking…";
          const res = await ctx.client.call("integration.setup", { id, fields, secrets });
          for (const [k, el] of Object.entries(inputs)) if (k.startsWith("s:")) el.value = "";
          if (res.need_more) {
            const more = res.need_more;
            return step(more.fields || [], more.secrets || [], more.message, { ...fields, _step: more.step });
          }
          account = res.account || "";
          confirmChange(ctx, res, done);
        }).catch((x) => showError(err, x));
      },
    }, ...fieldEls, ...secretEls, go);

    screen(
      header(ctx.hello),
      back && !message ? backBar("Settings", back) : null,
      h("div", { class: "title-row" }, badge(entry.name, 44), h("h1", {}, `Connect ${entry.name}`)),
      !message && entry.connected ? h("p", { class: "muted" }, "This adds another account. Signing in to an account that is already connected updates its password and keeps its settings.") : null,
      h("p", {}, message || entry.needs),
      form,
      err,
      h("p", { class: "muted" }, ((entry.egress || []).length
        ? `Rubi will connect only to ${entry.egress.join(", ")}. `
        : "This plugin connects to nothing on the internet itself. ") + "What you enter is stored encrypted on your Rubi and never shown to your agent."),
    );
  };
  step(entry.fields || [], entry.secrets || [], "", {});
}

// integrityLine shows whether the running Rubi binary matches the signed official release.
function integrityLine(st) {
  const i = st?.integrity;
  if (!i) return null;
  const text = { verified: `Official release ${i.version}, verified`, modified: `Warning: ${i.detail}`,
    unknown: `Release check: ${i.detail}` }[i.status] || i.detail;
  const line = h("p", { class: i.status === "modified" ? "error" : "muted" }, text);
  if (!st.downgraded_from) return line;
  return h("div", {}, line, h("p", { class: "error" },
    `Rubi is running ${st.version}, older than the ${st.downgraded_from} it ran before. If you didn't roll back on purpose, ask your agent to update Rubi.`));
}


// ---------- loading: a thin ruby line at the top while Rubi takes a moment ----------

let pending = 0;
let barTimer = 0;
const bar = h("div", { class: "topbar-progress", "aria-hidden": "true" });
document.body.append(bar);

// tracked wraps a client so every call shows the progress line after 250 ms.
function tracked(client) {
  if (client.tracked) return client;
  const call = client.call.bind(client);
  client.call = async (...args) => {
    if (pending++ === 0) barTimer = setTimeout(() => bar.classList.add("on"), 250);
    try {
      return await call(...args);
    } finally {
      if (--pending === 0) {
        clearTimeout(barTimer);
        bar.classList.remove("on");
      }
    }
  };
  client.tracked = true;
  return client;
}

// connecting is what the page shows while it reaches Rubi for the first time.
function connecting() {
  const note = h("p", { class: "muted center", "aria-live": "polite" }, "Connecting to your Rubi…");
  const sk = h("div", { class: "skeleton", "aria-hidden": "true" }, h("i", { class: "w60" }), h("i"), h("i", { class: "w80" }), h("i", { class: "w40" }));
  screen(face("thinking", 84), note, sk);
  setTimeout(() => { if (note.isConnected) note.textContent = "Still connecting… Your Rubi may be starting up; this can take a few seconds."; }, 5000);
  setTimeout(() => { if (note.isConnected) note.textContent = "Taking longer than usual. If it doesn't open, ask your agent for a new link."; }, 15000);
}

// ---------- small components ----------

// stepper shows where the user is in a short flow (setup: unlock method, backup, Bot).
function stepper(labels, current) {
  return h("ol", { class: "stepper", "aria-label": `Step ${current + 1} of ${labels.length}` }, labels.map((l, i) =>
    h("li", { class: i < current ? "done" : i === current ? "now" : "" , "aria-current": i === current ? "step" : null },
      h("span", {}, i < current ? icon("check", 12) : String(i + 1)), h("em", {}, l))));
}
const SETUP_STEPS = ["Unlock", "Password", "Your Bot"];

// pwField adds a show/hide button to a password input.
function pwField(input) {
  const eye = h("button", { type: "button", class: "pw-eye", "aria-label": "Show password", title: "Show password" }, icon("eye", 18));
  eye.onclick = () => {
    const show = input.type === "password";
    input.type = show ? "text" : "password";
    eye.setAttribute("aria-label", show ? "Hide password" : "Show password");
    eye.classList.toggle("on", show);
    input.focus();
  };
  return h("div", { class: "pw-wrap" }, input, eye);
}

// strength rates a new password roughly (length and variety); 0–4.
function strength(pw) {
  if (!pw) return 0;
  let score = pw.length >= 12 ? 2 : pw.length >= 8 ? 1 : 0;
  const kinds = [/[a-z]/, /[A-Z]/, /\d/, /[^\w\s]/].filter((r) => r.test(pw)).length;
  if (pw.length >= 16) score++;
  if (kinds >= 3) score++;
  return Math.min(4, score);
}

// copyButton copies text; its icon turns into a check for a moment.
function copyButton(text, label = "Copy", cls = "secondary small") {
  const b = h("button", { type: "button", class: `${cls} copy-btn` }, icon("copy", 16), h("span", {}, label));
  b.onclick = async () => {
    try {
      await navigator.clipboard.writeText(typeof text === "function" ? text() : text);
      b.replaceChildren(icon("check", 16), h("span", {}, "Copied"));
      b.classList.add("copied");
    } catch {
      b.replaceChildren(icon("copy", 16), h("span", {}, "Select and copy"));
    }
    setTimeout(() => { b.replaceChildren(icon("copy", 16), h("span", {}, label)); b.classList.remove("copied"); }, 1800);
  };
  return b;
}

// backBar is the way back, at the top of a screen that was opened from another one.
function backBar(label, onBack) {
  return h("button", { class: "back", type: "button", onclick: onBack }, icon("back", 18), label);
}

// badge is a plugin's mark: its initials on a hexagon, like Rubi.
function badge(name, size = 40) {
  const words = String(name || "?").replace(/^Unofficial /, "").split(/\s+/).filter(Boolean);
  const text = (words.length > 1 ? words[0][0] + words[1][0] : words[0].slice(0, 2)).toUpperCase();
  return h("span", { class: "pbadge", style: `--size:${size}px`, "aria-hidden": "true" }, text);
}

// menu is a "…" button with a small list of actions.
function menu(label, items) {
  const pop = h("div", { class: "menu-pop", role: "menu" }, items.filter(Boolean));
  const d = h("details", { class: "menu" }, h("summary", { "aria-label": label, title: label }, h("span", { class: "dots" }, "•••")), pop);
  d.addEventListener("toggle", () => {
    if (!d.open) return;
    const close = (e) => {
      if (!d.contains(e.target) || e.target.closest(".menu-pop button")) {
        d.open = false;
        removeEventListener("click", close, true);
      }
    };
    setTimeout(() => addEventListener("click", close, true));
  });
  return d;
}

// segmented is a row of choices, one selected; onChange gets the new value.
function segmented(options, value, onChange, disabled = false) {
  const wrap = h("div", { class: "seg" + (disabled ? " disabled" : ""), role: "radiogroup" });
  const name = "seg" + Math.random().toString(36).slice(2);
  for (const [v, label] of options) {
    const r = h("input", { type: "radio", name, value: v, checked: v === value, disabled, onchange: () => {
      wrap.style.setProperty("--i", String(options.findIndex((o) => o[0] === v)));
      onChange(v);
    } });
    wrap.append(h("label", {}, r, h("span", {}, label)));
  }
  wrap.style.setProperty("--n", String(options.length));
  wrap.style.setProperty("--i", String(Math.max(0, options.findIndex((o) => o[0] === value))));
  return wrap;
}

const SETTINGS_TABS = [
  ["plugins", "Plugins", "plug"],
  ["updates", "Updates", "download"],
  ["bots", "Grok Bot", "bot"],
  ["home", "Rubi Home", "home"],
  ["approvals", "Approvals", "shield"],
  ["security", "Security", "lock"],
];

async function settingsScreen(ctx, tab) {
  tab = tab || ctx.settingsTab || "plugins";
  ctx.settingsTab = tab;
  currentView = () => settingsScreen(ctx);
  const err = errorBox();
  let store, policy, hook, st, devices;
  try {
    [store, policy, hook, st, devices] = await Promise.all([
      ctx.client.call("store.list"), ctx.client.call("policy.get"),
      ctx.client.call("agents.get").catch(() => ctx.client.call("webhook.get")), ctx.client.call("status"),
      ctx.client.call("devices.get").catch(() => null), // older Rubi: no Rubi Home
    ]);
  } catch (e) {
    return fatal(e instanceof UserError ? e.message : friendly(e));
  }
  const back = () => settingsScreen(ctx);
  const change = (op, args) => async (e) => {
    const target = e.target.closest("button") || e.target;
    await busy(target, async () => confirmChange(ctx, await ctx.client.call(op, args()), back)).catch((x) => showError(err, x));
  };

  // ----- plugins
  const installed = (store.plugins || []).filter((p) => p.installed);
  // Each plugin lists its connected accounts (several mailboxes, for example), each with its own settings.
  // An older Rubi reports one account without a list.
  const accountsOf = (p) => p.accounts || (p.connected ? [{ id: "", label: p.account || "Connected", default: true }] : []);
  const pluginCard = (p) => {
    const accts = accountsOf(p);
    return h("article", { class: "pcard" },
      h("header", { class: "pcard-head" },
        badge(p.name),
        h("div", { class: "pcard-title" },
          h("strong", {}, p.name),
          h("span", { class: "muted small" }, p.version, " · ", h("span", { class: p.reviewed ? "ok" : "error" }, p.reviewed ? "Reviewed" : "Not reviewed"),
            p.running === false ? " · not running" : "")),
        menu(`More for ${p.name}`, [
          p.previous_version ? h("button", { type: "button", onclick: change("plugin.rollback", () => ({ id: p.id })) }, icon("undo", 16), `Roll back to ${p.previous_version}`) : null,
          h("button", { type: "button", class: "danger-item", onclick: change("plugin.remove", () => ({ id: p.id })) }, icon("trash", 16), "Remove plugin"),
        ])),
      p.update_available ? h("button", { class: "primary small update-cta", onclick: change("plugin.update", () => ({ id: p.id })) },
        icon("download", 16), `Update to ${p.update_available}`) : null,
      accts.length ? h("ul", { class: "acct-list" }, accts.map((a) => h("li", {},
        h("span", { class: "avatar-dot small" }, (a.label || a.id || "?").charAt(0).toUpperCase()),
        h("span", { class: "acct-name" }, a.label || a.id, a.default && accts.length > 1 ? h("span", { class: "tag" }, "Default") : null),
        p.has_config ? h("button", { class: "icon-btn", title: "Settings", "aria-label": `Settings for ${a.label || a.id}`,
          onclick: () => pluginConfigScreen(ctx, p.id, { account: a.id }) }, icon("gear", 18)) : null,
        menu(`More for ${a.label || a.id}`, [
          h("button", { type: "button", class: "danger-item", onclick: change("integration.disconnect", () => ({ id: p.id, account: a.id })) }, icon("close", 16), "Disconnect"),
        ])))) : h("p", { class: "muted small" }, "Not connected yet."),
      h("button", { class: "ghost small", onclick: () => setupScreen(ctx, p.id, back) }, icon("plus", 16), p.connected ? "Add account" : "Connect"));
  };

  // ----- approval levels, grouped by plugin
  const pending = {};
  const savePolicy = h("button", { class: "primary", disabled: true, onclick: change("policy.set", () => ({ levels: pending })) }, "Save approval levels");
  const groups = new Map();
  for (const a of policy.actions || []) {
    if (!groups.has(a.integration)) groups.set(a.integration, []);
    groups.get(a.integration).push(a);
  }
  const levelOpts = (policy.levels || []).map((l) => [l, { none: "Free", chat: "Chat", strong: "Passkey" }[l] || l]);
  const policyBlocks = [...groups].map(([name, acts]) => h("section", { class: "policy-group" },
    h("h3", {}, badge(name, 26), name),
    acts.map((a) => h("div", { class: "policy-row" },
      h("div", {}, h("span", {}, a.title), a.locked ? h("small", { class: "muted" }, icon("lock", 12), " Always needs your passkey") : null),
      segmented(levelOpts, a.level, (v) => {
        if (v === a.level) delete pending[a.kind];
        else pending[a.kind] = v;
        savePolicy.disabled = !Object.keys(pending).length;
      }, a.locked)))));

  // ----- sections
  const receipts = (st.receipts || []).slice(-5).reverse();
  const agents = hook.agents || [];
  const sections = {
    plugins: () => [
      h("div", { class: "section-head" }, h("h2", {}, "Plugins"),
        h("button", { class: "secondary small", onclick: () => storeScreen(ctx, back) }, icon("plus", 16), "Add from the store")),
      installed.length ? h("div", { class: "pgrid" }, installed.map(pluginCard))
        : h("div", { class: "empty" }, face("idle", 64), h("p", {}, "No plugins yet. Rubi starts bare; add what you need from the store.")),
    ],
    bots: () => [
      h("h2", {}, "Grok Bot"),
      h("p", { class: agents.length ? "muted" : "error" }, agents.length
        ? `${agents.length === 1 ? "1 Bot is" : agents.length + " Bots are"} connected. Rubi wakes them when something they follow happens.`
        : "No Bot is connected, so Rubi can't wake your agent. Setup isn't finished."),
      agents.length ? h("ul", { class: "acct-list" }, agents.map((a) => h("li", {},
        h("span", { class: "avatar-dot small" }, icon("bot", 14)), h("span", { class: "acct-name" }, a.name, a.default ? h("span", { class: "tag" }, "Default") : null)))) : null,
      h("button", { class: "secondary", onclick: () => grokBotScreen(ctx) }, "Manage Grok Bot connections"),
    ],
    home: () => [
      h("h2", {}, "Rubi Home"),
      h("p", { class: "muted" }, "A helper on a computer at home (a Mac that stays on) lets plugins control Philips Hue and run your Shortcuts."),
      devices?.devices?.length ? h("ul", { class: "acct-list" }, devices.devices.map((d) => {
        const state = h("span", { class: "status-dot", title: "Not checked" });
        return h("li", {}, h("span", { class: "avatar-dot small" }, icon("home", 14)), h("span", { class: "acct-name" }, d.name, state),
          h("button", { class: "ghost small", onclick: async (e) => {
            await busy(e.target, async () => {
              const r = await ctx.client.call("device.check", { id: d.id });
              state.className = "status-dot " + (r.online ? "on" : "off");
              state.title = r.online ? `Online · ${r.version || ""}` : "Not answering";
              state.textContent = r.online ? "Online" : "Not answering";
            }).catch((x) => showError(err, x));
          } }, "Check"),
          menu(`More for ${d.name}`, [h("button", { type: "button", class: "danger-item", onclick: change("device.remove", () => ({ id: d.id })) }, icon("trash", 16), "Remove")]));
      })) : null,
      h("button", { class: "secondary", onclick: () => homeDeviceScreen(ctx, back) }, icon("plus", 16), "Add a home computer"),
    ],
    approvals: () => [
      h("h2", {}, "Approvals"),
      h("p", { class: "muted" }, "How each action is approved: freely, with buttons in the agent chat, or with your passkey. Changing these always needs your passkey or password."),
      ...policyBlocks,
      policyBlocks.length ? savePolicy : h("p", { class: "muted" }, "Install a plugin to see its actions here."),
    ],
    security: () => [
      h("h2", {}, "Security"),
      integrityLine(st),
      receipts.length ? h("h3", {}, "Recent unlocks") : null,
      receipts.length ? h("ul", { class: "acct-list" }, receipts.map((r) => h("li", {}, h("span", { class: "avatar-dot small" }, icon(r.method === "passkey" ? "passkey" : "key", 14)),
        h("span", { class: "acct-name" }, `${r.event} with ${r.method}`, h("small", { class: "muted" }, fmtTime(r.at)))))) : null,
      h("p", { class: "muted small" }, "Don't recognize one of these? Lock Rubi and tell your agent."),
      h("button", { class: "danger", onclick: async (e) => {
        await busy(e.target, () => ctx.client.call("lock"));
        statusScreen(ctx, "Rubi is locked.");
      } }, icon("lock", 18), "Lock Rubi now"),
    ],
  };
  if (!devices) delete sections.home;
  if (!sections[tab]) tab = "plugins";

  const body = h("div", { class: "settings-body" });
  const nav = h("nav", { class: "settings-nav", "aria-label": "Settings" });
  const show = (id) => {
    ctx.settingsTab = id;
    for (const b of nav.querySelectorAll("button")) b.setAttribute("aria-current", String(b.dataset.tab === id));
    requestAnimationFrame(() => nav.querySelector("[aria-current=true]")?.scrollIntoView({ inline: "center", block: "nearest", behavior: "smooth" }));
    body.replaceChildren(...sections[id]());
    body.querySelectorAll(":scope > *").forEach((el, i) => {
      el.classList.remove("rise");
      void el.offsetWidth;
      el.classList.add("rise");
      el.style.setProperty("--d", `${Math.min(i, 8) * 35}ms`);
    });
  };
  for (const [id, label, ic] of SETTINGS_TABS) {
    if (id === "updates") { // its own page; the count shows here
      const count = h("i", { class: "nav-count", hidden: true });
      nav.append(h("button", { type: "button", "data-tab": id, onclick: () => updatesScreen(ctx) }, icon(ic, 18), h("span", {}, label), count));
      loadUpdates().then((items) => {
        const n = outdated(items);
        if (n) { count.textContent = String(n); count.hidden = false; }
      }).catch(() => {});
      continue;
    }
    if (!sections[id]) continue;
    nav.append(h("button", { type: "button", "data-tab": id, onclick: () => show(id) }, icon(ic, 18), h("span", {}, label),
      id === "bots" && !agents.length ? h("i", { class: "nav-dot", title: "Needs attention" }) : null,
      null));
  }

  onSettings = true;
  const head = header(ctx.hello);
  onSettings = false;
  screen(
    { cls: "wide settings" },
    head,
    h("h1", {}, "Settings"),
    err,
    h("div", { class: "settings-layout" }, nav, body),
  );
  show(tab);
}

const HOME_INSTALL = "curl -fsSL https://rubi-panel.com/install-home.sh | sh";

// homeDeviceScreen pairs a Rubi Home helper: install it on the computer at home, paste its code.
function homeDeviceScreen(ctx, back) {
  const err = errorBox();
  const code = h("textarea", { rows: "4", placeholder: "rubi-home:…", autocapitalize: "none", spellcheck: "false" });
  const copy = copyButton(HOME_INSTALL);
  const pair = h("button", { class: "primary", onclick: async (e) => {
    await busy(e.target, async () => {
      e.target.textContent = "Reaching Rubi Home…";
      const res = await ctx.client.call("device.pair", { code: code.value.trim() });
      confirmChange(ctx, res, back);
    }).catch((x) => showError(err, x));
  } }, "Pair");
  screen(
    header(ctx.hello),
    backBar("Settings", back),
    h("h1", {}, "Add a home computer"),
    h("p", {}, "Rubi runs on your agent's machine and can't reach your home network. Rubi Home, a small helper on a computer at home, can: it talks to your Hue Bridge and runs the shortcuts you put in its folder."),
    h("ol", { class: "steps-list" },
      h("li", {}, "On a Mac at home that stays on (or a Linux box), open Terminal and run:",
        h("div", { class: "cmd" }, h("code", {}, HOME_INSTALL), copy)),
      h("li", {}, "It prints a pairing code starting with rubi-home:. Paste it here. (Later: rubi-home pair makes a new one.)"),
      h("li", {}, "For Apple Home, make a folder named Rubi in the Shortcuts app and put there only the shortcuts Rubi may run.")),
    h("label", { class: "field" }, h("span", {}, "Pairing code"), code),
    err,
    pair,
  );
}

// The message the user sends to each Grok Bot that should hear from Rubi.
const CONNECT_PROMPT = "Connect yourself to Rubi so it can wake you: create a routine named \"Rubi events\" " +
  "with a webhook trigger and no schedule, whose instruction is \"A Rubi event arrived. Follow next_step in the " +
  "JSON body.\" Then send me rubi_link(\"agent:<your Bot name>\"). Once I've connected it, choose which Rubi " +
  "notifications you need with rubi_notifications.";


// grokBotScreen manages the Bots Rubi can wake: one webhook per Bot, any number of them, each with the
// notifications it hears about.
async function grokBotScreen(ctx) {
  currentView = () => grokBotScreen(ctx);
  const err = errorBox();
  let data;
  try {
    data = await ctx.client.call("agents.get");
  } catch (e) {
    return fatal(e instanceof UserError ? e.message : friendly(e));
  }
  const again = () => grokBotScreen(ctx);
  const back = () => settingsScreen(ctx);
  const sources = data.sources || [];
  const change = (op, args) => async (e) => {
    await busy(e.target, async () => confirmChange(ctx, await ctx.client.call(op, args), again)).catch((x) => showError(err, x));
  };

  const prompt = h("div", { class: "prompt" }, CONNECT_PROMPT);
  const copy = copyButton(CONNECT_PROMPT, "Copy the message");

  const cards = (data.agents || []).map((a) => {
    const subs = new Set(a.subscriptions || []);
    const saved = h("span", { class: "muted small" });
    const boxes = sources.map((src) => {
      const box = h("input", { type: "checkbox", checked: subs.has(src.id) });
      box.addEventListener("change", async () => {
        const want = sources.filter((x) => x.id === src.id ? box.checked : subs.has(x.id)).map((x) => x.id);
        saved.textContent = "Saving…";
        try {
          const res = await ctx.client.call("agent.subscribe", { name: a.name, sources: want });
          subs.clear();
          for (const x of res.subscriptions) subs.add(x);
          saved.textContent = "Saved";
        } catch (x) {
          box.checked = !box.checked;
          saved.textContent = "";
          showError(err, x);
        }
      });
      return h("label", { class: "pill-toggle" }, box, h("span", {}, src.name));
    });
    return h("div", { class: "agent-card" },
      h("div", { class: "agent-head" },
        h("span", { class: "avatar-dot" }, icon("bot", 16)),
        h("div", { class: "agent-name" }, h("strong", {}, a.name), " ", a.default ? h("span", { class: "tag" }, "Default") : null,
          h("div", { class: "muted small" }, a.host)),
        h("div", { class: "actions" },
          h("button", { class: "ghost small", onclick: async (e) => {
            const b = e.currentTarget;
            await busy(b, () => ctx.client.call("webhook.test", { name: a.name }))
              .then(() => { b.textContent = "Sent"; }).catch((x) => showError(err, x));
          } }, "Test"),
          menu(`More for ${a.name}`, [
            a.default ? null : h("button", { type: "button", onclick: change("agent.default", { name: a.name }) }, icon("check", 16), "Make default"),
            h("button", { type: "button", class: "danger-item", onclick: change("agent.remove", { name: a.name }) }, icon("trash", 16), "Remove"),
          ]))),
      h("p", { class: "muted small" }, "Wakes this Bot for:"),
      h("div", { class: "pills" }, boxes), saved);
  });

  screen(
    { cls: "wide", focus: false },
    header(ctx.hello),
    backBar("Settings", back),
    h("h1", {}, "Grok Bot connections"),
    h("p", { class: "muted" }, "Rubi wakes a Grok Bot through that Bot's own routine webhook: when you approve something it asked for, or when something it follows happens (like a reply to a tracked email). The routine runs only then, never on a schedule."),
    err,
    h("div", { class: "section-head" }, h("h2", {}, "Connected Bots")),
    cards.length ? h("div", { class: "agent-list" }, cards) : h("div", { class: "empty" }, face("concern", 56), h("p", {}, "None yet. Setup isn't finished until at least one Bot is connected.")),
    cards.length ? h("p", { class: "muted small" }, "Results of approvals always go to the Bot that asked. Notifications no Bot chose go to the default Bot.") : null,
    h("div", { class: "section-head" }, h("h2", {}, "Add a Bot")),
    h("div", { class: "panes" },
      pane(h("h3", {}, h("span", { class: "num-s" }, "A"), "Ask the Bot to connect itself"),
        h("p", { class: "muted" }, "Send this to each Grok Bot that should hear from Rubi. It creates the routine and sends you a link; open that link in the Grok Bot desktop app, where the webhook is shown."),
        prompt, copy),
      pane(h("h3", {}, h("span", { class: "num-s" }, "B"), "Or add a webhook here"),
        agentHowTo(""),
        agentForm(ctx, { err, onSaved: () => again() }),
        h("p", { class: "footnote" }, "* Name each webhook exactly like its Grok Bot. Bots identify themselves to Rubi by name, so they can then choose for themselves which notifications they receive. You can always change it above."))),
  );
}

// pluginConfigScreen edits a plugin's user-only settings (like a privacy filter). Only the user can change
// them, with the passkey or the password; the agent can't.
async function pluginConfigScreen(ctx, id, opts = {}) {
  const err = errorBox();
  let cfg;
  try {
    cfg = await ctx.client.call("plugin.config.get", { id, account: opts.account || "" });
  } catch (e) {
    return fatal(e instanceof UserError ? e.message : friendly(e));
  }
  const back = opts.done || (() => settingsScreen(ctx));
  const inputs = {};
  const several = (cfg.accounts || []).length > 1;
  const rows = (cfg.fields || []).map((f) => {
    const v = cfg.values[f.key];
    if (several && !f.per_account) f = { ...f, help: [f.help, "Applies to all accounts."].filter(Boolean).join(" ") };
    let el;
    if (f.type === "info") {
      return h("div", { class: "field info" }, h("span", { class: "label" }, f.label), f.help ? h("small", {}, f.help) : null,
        ...(f.options || []).map((o) => {
          const copy = h("button", { class: "secondary small", onclick: async () => {
            try {
              await navigator.clipboard.writeText(o.key);
              copy.textContent = "Copied";
            } catch {
              copy.textContent = "Select and copy";
            }
          } }, "Copy");
          return h("div", { class: "info-row" }, h("span", {}, o.label), h("code", {}, o.key), copy);
        }));
    }
    if (f.type === "choice") {
      const name = "cfg-" + f.key;
      const radios = (f.options || []).map((o) => {
        const r = h("input", { type: "radio", name, value: o.key, checked: v === o.key });
        return { r, row: h("label", { class: "check" }, r, h("span", {}, o.label)) };
      });
      inputs[f.key] = () => (radios.find((x) => x.r.checked) || {}).r?.value ?? v;
      return h("fieldset", { class: "field" }, h("legend", {}, f.label), h("div", { class: "checks" }, radios.map((x) => x.row)),
        f.help ? h("small", {}, f.help) : null);
    }
    if (f.type === "list" && (f.options || []).length) {
      const have = new Set(v || []);
      const boxes = f.options.map((o) => {
        const b = h("input", { type: "checkbox", value: o.key, checked: have.has(o.key) });
        return { b, row: h("label", { class: "check" }, b, h("span", {}, o.label)) };
      });
      inputs[f.key] = () => boxes.filter((x) => x.b.checked).map((x) => x.b.value);
      return h("fieldset", { class: "field" }, h("legend", {}, f.label), h("div", { class: "checks" }, boxes.map((x) => x.row)),
        f.help ? h("small", {}, f.help) : null);
    }
    if (f.type === "bool") {
      el = h("input", { type: "checkbox", checked: !!v });
      inputs[f.key] = () => el.checked;
      return h("label", { class: "check" }, el, h("span", {}, f.label, f.help ? h("small", { class: "muted small block" }, f.help) : null));
    }
    if (f.type === "list") {
      el = h("textarea", { rows: "5", autocapitalize: "none", spellcheck: "false" });
      el.value = (v || []).join("\n");
      inputs[f.key] = () => el.value.split("\n").map((x) => x.trim()).filter(Boolean);
    } else {
      el = h("input", { type: "text", value: v || "" });
      inputs[f.key] = () => el.value;
    }
    return h("label", { class: "field" }, h("span", {}, f.label), el, f.help ? h("small", {}, f.help) : null);
  });
  const snapshot = () => JSON.stringify(Object.fromEntries(Object.entries(inputs).map(([k, get]) => [k, get()])));
  let initial = "";
  const bar = h("div", { class: "savebar" + (opts.done ? " always" : "") });
  const markDirty = () => bar.classList.toggle("dirty", snapshot() !== initial);
  const save = h("button", { class: "primary", onclick: async (e) => {
    const values = {};
    for (const [k, get] of Object.entries(inputs)) values[k] = get();
    await busy(e.target, async () => {
      let res;
      try {
        res = await ctx.client.call("plugin.config.set", { id, values, account: cfg.account || "" });
      } catch (x) {
        if (opts.done && /nothing changed/i.test(x.message)) return opts.done(); // defaults are fine
        throw x;
      }
      confirmChange(ctx, res, opts.done || (() => pluginConfigScreen(ctx, id, opts)), opts.done ? "Finish" : undefined);
    }).catch((x) => showError(err, x));
  } }, opts.done ? "Save and finish" : "Save");
  bar.append(h("span", { class: "savebar-note" }, opts.done ? "You can change these later in Settings." : "Unsaved changes"),
    opts.done ? h("button", { class: "link", type: "button", onclick: back }, "Keep the defaults") : null, save);
  const account = ((cfg.accounts || []).find((a) => a.id === cfg.account) || {}).label || cfg.account;
  screen(
    { focus: false },
    header(ctx.hello),
    opts.done ? null : backBar("Settings", back),
    opts.intro ? h("p", { class: "eyebrow" }, "Last step") : null,
    h("div", { class: "title-row" }, badge(cfg.name, 44), h("div", {},
      h("h1", {}, opts.intro ? `What can your agent see in ${cfg.name}?` : `${cfg.name} settings`),
      (cfg.accounts || []).length > 1 || opts.intro ? h("p", { class: "muted" }, account) : null)),
    h("p", { class: "note" }, icon("lock", 16), "Only you can change these, with your passkey or password. Your agent can't read or change them."),
    err,
    h("div", { class: "cfg" }, rows),
    bar,
  );
  initial = snapshot();
  root.querySelector(".cfg")?.addEventListener("input", markDirty);
  root.querySelector(".cfg")?.addEventListener("change", markDirty);
}

// ---------- store ----------

async function storeScreen(ctx, back) {
  currentView = () => storeScreen(ctx, back);
  const err = errorBox();
  let store;
  try {
    store = await ctx.client.call("store.list");
  } catch (e) {
    return fatal(e instanceof UserError ? e.message : friendly(e));
  }
  const again = () => storeScreen(ctx, back);
  const install = (plugin) => async (e) => {
    const target = e.target.closest("button") || e.target;
    await busy(target, async () => {
      confirmChange(ctx, await ctx.client.call("plugin.install", { plugin }), again);
    }).catch((x) => showError(err, x));
  };
  const reviewed = (store.plugins || []).filter((p) => p.reviewed && (p.latest || p.requires_newer_rubi));
  const tiles = reviewed.map((p) => {
    const action = p.installed
      ? (p.update_available
        ? h("button", { class: "primary small", onclick: async (e) => {
          await busy(e.target, async () => confirmChange(ctx, await ctx.client.call("plugin.update", { id: p.id }), again)).catch((x) => showError(err, x));
        } }, icon("download", 16), "Update")
        : h("span", { class: "tag" }, icon("check", 12), "Installed"))
      : p.requires_newer_rubi
        ? h("span", { class: "tag warn" }, "Needs a newer Rubi")
        : h("button", { class: "secondary small", onclick: install(p.id) }, icon("plus", 16), "Install");
    return h("article", { class: "stile", "data-q": `${p.name} ${p.summary || ""} ${p.publisher || ""}`.toLowerCase() },
      badge(p.name, 48),
      h("div", { class: "stile-text" },
        h("strong", {}, p.name),
        h("p", { class: "muted" }, p.summary || ""),
        h("span", { class: "muted small" }, icon("shield", 12), ` ${p.publisher || "Rubi-Project"}${p.latest ? ` · ${p.latest}` : ""}`)),
      h("div", { class: "stile-action" }, action));
  });
  const grid = h("div", { class: "sgrid" }, tiles);
  const none = h("p", { class: "muted center", hidden: true }, "No plugin matches.");
  const search = h("input", { type: "search", placeholder: "Search plugins", "aria-label": "Search plugins", autocomplete: "off",
    oninput: () => {
      const q = search.value.trim().toLowerCase();
      let shown = 0;
      for (const t of tiles) {
        const hit = !q || t.dataset.q.includes(q);
        t.classList.toggle("is-hidden", !hit);
        shown += hit;
      }
      none.hidden = shown > 0;
    } });

  const url = h("input", { type: "url", placeholder: "https://github.com/owner/repo", autocomplete: "off", autocapitalize: "none" });
  const sideload = h("details", { class: "sideload" },
    h("summary", {}, icon("link", 16), "Install from a link"),
    h("p", { class: "muted" }, "Plugins outside the store are not reviewed by Rubi-Project. Install one only if you trust who made it: it runs on your agent's computer with access to what you enter for it."),
    url,
    h("button", { class: "secondary", onclick: (e) => install(url.value.trim())(e) }, "Check and install"));

  screen(
    { cls: "wide", focus: false },
    header(ctx.hello),
    back ? backBar("Settings", back) : null,
    h("h1", {}, "Plugin store"),
    h("p", { class: "muted" }, "Plugins reviewed by Rubi-Project. Installing one shows what it can do and needs your passkey or password."),
    err,
    store.catalog_error ? h("p", { class: "error" }, `The store is unavailable right now: ${store.catalog_error}`) : null,
    tiles.length > 4 ? h("div", { class: "search" }, icon("search", 18), search) : null,
    tiles.length ? grid : (store.catalog_error ? null : h("p", { class: "muted" }, "The store is empty.")),
    none,
    sideload,
  );
}

main().catch((e) => fatal(friendly(e)));
// Opening a new link while the panel is already open only changes the fragment.
window.addEventListener("hashchange", () => main().catch((e) => fatal(friendly(e))));
