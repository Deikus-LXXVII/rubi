// Rubi panel: pairing, unlocking and approving. A static page; all state lives on the user's Rubi
// instance and, for pinning, in this browser's localStorage.

import { RubiClient, UserError, parseLink } from "./api.js";
import {
  approveKey, b64u, hmacSha256, newPasswordKdf, passwordKek, prfKek, rand, supportsX25519, unwrapDek, wrapDek,
} from "./crypto.js";
import { createPasskey, evalPrf, passkeysAvailable, signChallenge } from "./passkey.js";

const root = document.getElementById("app");

// ---------- tiny DOM helpers (text only: nothing from Rubi or third parties is parsed as HTML) ----------

function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

function screen(...children) {
  root.replaceChildren(h("main", { class: "card" }, ...children));
  root.querySelector("input")?.focus();
}

function header(hello) {
  return h("div", { class: "instance" }, h("span", { class: "dot" }), `Rubi · ${hello.fingerprint}`);
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
  const label = button.textContent;
  button.disabled = true;
  button.textContent = "Working…";
  try {
    return await fn();
  } finally {
    button.disabled = false;
    button.textContent = label;
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

// ---------- entry ----------

async function main() {
  if (!window.isSecureContext) return fatal("This page must be opened over HTTPS.");
  const link = parseLink(location.hash);
  // Keep the one-time codes out of the address bar and history.
  if (location.hash) history.replaceState(null, "", location.pathname + location.search);
  if (!link) return home();
  if (!(await supportsX25519())) {
    return fatal("This browser is too old for Rubi's encryption. Update it (Safari 17+, Chrome 133+, Firefox 130+).");
  }

  screen(h("p", { class: "muted" }, "Connecting to your Rubi…"));
  const client = new RubiClient(link);
  let hello;
  try {
    hello = await client.call("hello");
  } catch (e) {
    return fatal(e instanceof UserError ? e.message : friendly(e));
  }

  const pin = loadPins()[hello.instance];
  if (pin && pin.k !== link.k) {
    return fatal("This link claims to be a Rubi you've used before, but its key is different. " +
      "Someone may be impersonating it. Don't continue; tell your agent.", true);
  }
  const ctx = { client, link, hello, pin };

  if (link.a === "pair") return pairScreen(ctx);
  if (hello.state === "unpaired") return fatal("This Rubi hasn't been set up yet. Ask your agent for a setup link.");
  if (link.a.startsWith("approve:")) return approveScreen(ctx, link.a.slice("approve:".length));
  if (link.a === "unlock") return unlockScreen(ctx);
  if (link.a.startsWith("setup:")) return whenUnlocked(ctx, () => setupScreen(ctx, link.a.slice("setup:".length)));
  if (link.a === "settings") return whenUnlocked(ctx, () => settingsScreen(ctx));
  return statusScreen(ctx, "");
}

function home() {
  screen(
    h("h1", {}, "Rubi"),
    h("p", {}, "This is the control panel for Rubi, which lets your AI agent use your accounts only with your approval."),
    h("p", { class: "muted" }, "Open the link your agent sent you. This page stores nothing on any server."),
  );
}

function fatal(message, danger = false) {
  screen(h("h1", {}, danger ? "Stop" : "Something's wrong"), h("p", { class: danger ? "error" : "" }, message));
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
    const form = h("form", {
      onsubmit: (e) => {
        e.preventDefault();
        if (pw.value.length < 12) return showError(err2, new UserError("Use at least 12 characters."));
        if (pw.value !== pw2.value) return showError(err2, new UserError("The passwords don't match."));
        busy(go, () => finish(pw.value)).catch((x) => showError(err2, x));
      },
    }, user, pw, pw2, go);
    screen(
      header(hello),
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
    screen(
      header(hello),
      h("h1", {}, "Rubi is set up"),
      h("p", {}, "It's unlocked and ready. You can close this page and go back to your agent."),
      h("p", { class: "muted" }, "After a restart Rubi locks itself again, and your agent will send you an unlock link."),
    );
  }

  screen(
    header(hello),
    h("h1", {}, "Set up Rubi"),
    h("p", {}, "Rubi will be locked with a key only you hold. Choose how you'll unlock it and approve your agent's actions."),
    h("p", { class: "muted" }, `Instance ${hello.instance}`),
    passkeysAvailable()
      ? h("button", { class: "primary", onclick: (e) => addPasskey(e.target) }, "Use Face ID / Touch ID")
      : null,
    h("button", { class: passkeysAvailable() ? "secondary" : "primary", onclick: passwordStep }, "Use a password only"),
    err,
  );
}

// ---------- unlocking ----------

// unlockFlow renders the unlock UI and calls onDone() once Rubi is unlocked.
async function unlockFlow(ctx, title, intro, onDone) {
  const { client, hello } = ctx;
  const keys = await client.call("unlock.keys");
  const err = errorBox();
  const passkeyWraps = keys.wraps.filter((w) => w.kind === "passkey");
  const passwordWraps = keys.wraps.filter((w) => w.kind === "password");

  async function send(dek, method, credentialId) {
    const res = await client.call("unlock", { dek: b64u.enc(dek), min_vault_version: ctx.pin?.maxVersion || 0, method,
      credential_id: credentialId });
    savePin(hello.instance, ctx.link.k, res.vault_version);
    await onDone();
  }

  async function withPasskey(btn) {
    await busy(btn, async () => {
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
  }, user, pw, go) : null;

  screen(
    header(hello),
    h("h1", {}, title),
    h("p", {}, intro),
    passkeyWraps.length && passkeysAvailable()
      ? h("button", { class: "primary", onclick: (e) => withPasskey(e.target) }, "Unlock with Face ID / Touch ID")
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
  let st = null;
  try {
    st = await ctx.client.call("status");
  } catch {
    /* status is informational */
  }
  const receipts = (st?.receipts || []).slice(-5).reverse();
  screen(
    header(ctx.hello),
    h("h1", {}, st?.state === "unlocked" ? "Rubi is unlocked" : "Rubi"),
    h("p", {}, message),
    receipts.length ? h("h2", {}, "Recent unlocks") : null,
    receipts.length ? h("ul", { class: "receipts" }, receipts.map((r) =>
      h("li", {}, `${fmtTime(r.at)} · ${r.event} with ${r.method}`))) : null,
    receipts.length ? h("p", { class: "muted" }, "Don't recognize one of these? Lock Rubi and tell your agent.") : null,
    st?.state === "unlocked"
      ? h("button", { class: "secondary", onclick: async (e) => {
        await busy(e.target, () => ctx.client.call("lock"));
        statusScreen(ctx, "Rubi is locked.");
      } }, "Lock Rubi now")
      : null,
  );
}

// ---------- approvals ----------

const FIELD_LABELS = {
  from: "From", to: "To", cc: "Cc", bcc: "Bcc", subject: "Subject", in_reply_to: "In reply to", body: "Message",
  integration: "Integration", account: "Account", connects_to: "Connects to", effect: "Effect", webhook: "Webhook",
};

function previewTable(preview) {
  if (!preview || typeof preview !== "object") return h("pre", { class: "body" }, JSON.stringify(preview, null, 2));
  const rows = [];
  // Familiar order first (Go sorts map keys alphabetically), then anything else.
  const order = Object.keys(FIELD_LABELS);
  const keys = Object.keys(preview).sort((a, b) =>
    (order.indexOf(a) + 1 || 99) - (order.indexOf(b) + 1 || 99) || a.localeCompare(b));
  for (const k of keys) {
    const v = preview[k];
    if (v === null || v === undefined || v === "") continue;
    const value = typeof v === "string" ? v : JSON.stringify(v, null, 2);
    rows.push(h("div", { class: "row" }, h("dt", {}, FIELD_LABELS[k] || k),
      h("dd", {}, k === "body" ? h("pre", { class: "body" }, value) : value)));
  }
  return h("dl", { class: "preview" }, rows);
}

// approveScreen shows one pending approval. opts.onDone (for settings changes) adds a way back.
async function approveScreen(ctx, id, opts = {}) {
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
  if (a.state !== "pending") return resultScreen(ctx, a, opts.onDone);

  const err = errorBox();
  const approverIds = (info.approvers || []).map((x) => x.credential_id);
  const usePasskey = () => approverIds.length > 0 && passkeysAvailable() && pwBox.hidden;
  const pwInput = h("input", { type: "password", autocomplete: "current-password", placeholder: "Your Rubi password" });
  const pwUser = h("input", { type: "text", autocomplete: "username", value: `Rubi ${ctx.hello.fingerprint}`, hidden: true, readonly: true });
  const pwBox = h("form", { onsubmit: (e) => e.preventDefault() }, pwUser, pwInput);
  pwBox.hidden = approverIds.length > 0 && passkeysAvailable();

  async function passwordProof(option) {
    const pw = pwInput.value;
    if (!pw) throw new UserError("Enter your password first.");
    const keys = await ctx.client.call("unlock.keys");
    for (const w of keys.wraps.filter((x) => x.kind === "password")) {
      const kek = await passwordKek(pw, w.kdf);
      try {
        await unwrapDek(kek, w, keys.instance); // proves the password before using it
      } catch {
        continue;
      }
      const mac = await hmacSha256(await approveKey(kek), b64u.dec(info.challenges[option]));
      return { type: "password", mac: b64u.enc(mac) };
    }
    throw new UserError("Wrong password.");
  }

  async function decide(btn, option) {
    await busy(btn, async () => {
      const proof = usePasskey()
        ? await signChallenge(b64u.dec(info.challenges[option]), approverIds)
        : await passwordProof(option);
      resultScreen(ctx, await ctx.client.call("approval.decide", { approval_id: id, option, approve: true, proof }), opts.onDone);
    }).catch((e) => showError(err, e));
  }

  screen(
    header(ctx.hello),
    h("p", { class: "eyebrow" }, opts.eyebrow || "Your agent is asking for approval"),
    h("h1", {}, a.summary),
    previewTable(a.preview),
    a.question ? h("p", { class: "question" }, a.question) : null,
    info.password ? pwBox : null,
    h("div", { class: "actions" }, a.options.map((o, i) =>
      h("button", { class: i === 0 ? "primary" : "secondary", onclick: (e) => decide(e.target, o.key) }, o.label))),
    h("button", { class: "danger", onclick: async (e) => {
      await busy(e.target, async () => resultScreen(ctx,
        await ctx.client.call("approval.decide", { approval_id: id, approve: false }), opts.onDone)).catch((x) => showError(err, x));
    } }, opts.declineLabel || "Decline"),
    !pwBox.hidden || !info.password ? null : h("button", { class: "link", onclick: (e) => {
      pwBox.hidden = false;
      e.target.remove();
      pwInput.focus();
    } }, "Use password instead"),
    err,
    h("p", { class: "muted" }, `Expires ${fmtTime(a.expires_at)}. Request ${a.approval_id}.`),
  );
}

function resultScreen(ctx, a, onDone) {
  const titles = {
    executed: "Done",
    denied: "Declined",
    failed: "It didn't work",
    expired: "This request expired",
    cancelled: "This request was cancelled",
  };
  screen(
    header(ctx.hello),
    h("h1", {}, titles[a.state] || a.state),
    h("p", {}, a.summary),
    a.error ? h("p", { class: "error" }, a.error) : null,
    onDone
      ? h("button", { class: "primary", onclick: onDone }, "Back to settings")
      : h("p", { class: "muted" }, a.state === "executed"
        ? "Your agent will see the result. You can close this page."
        : "Nothing was done. You can close this page."),
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

// confirmChange takes the {approval_id, ticket} a settings operation returns and asks for Face ID or the
// password, using a client bound to that approval's ticket.
function confirmChange(ctx, res, onDone) {
  const approvalCtx = { ...ctx, client: new RubiClient({ ...ctx.link, t: res.ticket }) };
  return approveScreen(approvalCtx, res.approval_id, { eyebrow: "Confirm this change", declineLabel: "Cancel", onDone });
}

async function setupScreen(ctx, id, back) {
  const err = errorBox();
  let entry;
  try {
    entry = (await ctx.client.call("integration.catalog")).integrations.find((i) => i.id === id);
  } catch (e) {
    return fatal(e instanceof UserError ? e.message : friendly(e));
  }
  if (!entry) return fatal("Unknown integration.");
  const done = back || (() => screen(header(ctx.hello), h("h1", {}, `${entry.name} is connected`),
    h("p", {}, "You can close this page and go back to your agent.")));

  const inputs = {};
  const fieldEls = (entry.fields || []).map((f) => {
    inputs["f:" + f.key] = h("input", { type: f.type === "email" ? "email" : "text", placeholder: f.placeholder || "",
      autocomplete: f.type === "email" ? "email" : "off", required: f.required, autocapitalize: "none" });
    return h("label", { class: "field" }, h("span", {}, f.label), inputs["f:" + f.key], f.help ? h("small", {}, f.help) : null);
  });
  const secretEls = (entry.secrets || []).map((sec) => {
    inputs["s:" + sec.key] = h("input", { type: "password", autocomplete: "off", required: true, autocapitalize: "none", spellcheck: "false" });
    return h("label", { class: "field" }, h("span", {}, sec.label), inputs["s:" + sec.key],
      sec.help ? h("small", {}, sec.help) : null,
      sec.help_url ? h("a", { href: sec.help_url, target: "_blank", rel: "noopener noreferrer", class: "button-link" }, "Open account.apple.com") : null);
  });
  const go = h("button", { class: "primary", type: "submit" }, `Connect ${entry.name}`);
  const form = h("form", {
    onsubmit: (e) => {
      e.preventDefault();
      busy(go, async () => {
        const fields = {}, secrets = {};
        for (const [k, el] of Object.entries(inputs)) {
          (k.startsWith("f:") ? fields : secrets)[k.slice(2)] = el.value;
        }
        go.textContent = "Checking your login…";
        const res = await ctx.client.call("integration.setup", { id, fields, secrets });
        for (const el of Object.values(inputs)) el.value = "";
        confirmChange(ctx, res, done);
      }).catch((x) => showError(err, x));
    },
  }, ...fieldEls, ...secretEls, go);

  screen(
    header(ctx.hello),
    h("h1", {}, `Connect ${entry.name}`),
    entry.connected ? h("p", { class: "ok" }, `Connected as ${entry.account}. Connecting again replaces it.`) : null,
    h("p", {}, entry.needs),
    form,
    err,
    h("p", { class: "muted" }, `Rubi will connect only to ${(entry.egress || []).join(", ")}. The password is stored encrypted on your Rubi and never shown to your agent.`),
    back ? h("button", { class: "link", onclick: back }, "Back to settings") : null,
  );
}

const LEVEL_LABELS = { none: "No approval", chat: "Buttons in chat", strong: "Face ID / password" };

async function settingsScreen(ctx) {
  const err = errorBox();
  let catalog, policy, hook, st;
  try {
    [catalog, policy, hook, st] = await Promise.all([
      ctx.client.call("integration.catalog"), ctx.client.call("policy.get"),
      ctx.client.call("webhook.get"), ctx.client.call("status"),
    ]);
  } catch (e) {
    return fatal(e instanceof UserError ? e.message : friendly(e));
  }
  const back = () => settingsScreen(ctx);
  const change = (op, args) => async (e) => {
    await busy(e.target, async () => confirmChange(ctx, await ctx.client.call(op, args()), back)).catch((x) => showError(err, x));
  };

  const integrationsList = h("div", { class: "list" }, catalog.integrations.map((i) => h("div", { class: "item" },
    h("div", {}, h("strong", {}, i.name), h("div", { class: "muted" }, i.connected ? `Connected: ${i.account}` : "Not connected")),
    i.connected
      ? h("button", { class: "secondary small", onclick: change("integration.disconnect", () => ({ id: i.id })) }, "Disconnect")
      : h("button", { class: "secondary small", onclick: () => setupScreen(ctx, i.id, back) }, "Connect"))));

  const selects = {};
  const policyRows = policy.actions.map((a) => {
    const sel = h("select", { disabled: a.locked }, policy.levels.map((l) =>
      h("option", { value: l, selected: l === a.level }, LEVEL_LABELS[l] + (l === a.default ? " (default)" : ""))));
    selects[a.kind] = { sel, current: a.level };
    return h("label", { class: "field row-field" }, h("span", {}, `${a.title}`, h("small", {}, a.integration)), sel);
  });
  const savePolicy = h("button", { class: "secondary", onclick: change("policy.set", () => {
    const levels = {};
    for (const [kind, { sel, current }] of Object.entries(selects)) if (sel.value !== current) levels[kind] = sel.value;
    return { levels };
  }) }, "Save approval levels");

  const hookUrl = h("input", { type: "url", placeholder: "https://… (from your Grok Bot routine)", autocomplete: "off", autocapitalize: "none" });
  const hookKey = h("input", { type: "password", placeholder: "Routine key (crsr_…)", autocomplete: "off", autocapitalize: "none" });

  const receipts = (st.receipts || []).slice(-5).reverse();
  screen(
    header(ctx.hello),
    h("h1", {}, "Rubi settings"),
    err,
    h("h2", {}, "Integrations"),
    integrationsList,
    h("h2", {}, "Approval levels"),
    h("p", { class: "muted" }, "How each action is approved. Changing these always needs Face ID or your password."),
    ...policyRows,
    savePolicy,
    h("h2", {}, "Agent webhook"),
    h("p", { class: "muted" }, hook.configured
      ? `Events go to ${hook.url}`
      : "Not set. Create a routine with a webhook trigger in Grok Bot and paste its URL and key here, so Rubi can wake your agent when something happens (e.g. a reply arrives)."),
    hookUrl, hookKey,
    h("button", { class: "secondary", onclick: change("webhook.set", () => ({ url: hookUrl.value.trim(), key: hookKey.value.trim() })) }, "Save webhook"),
    hook.configured ? h("button", { class: "link", onclick: async (e) => {
      await busy(e.target, () => ctx.client.call("webhook.test")).then(() => { e.target.textContent = "Test event sent"; }).catch((x) => showError(err, x));
    } }, "Send a test event") : null,
    hook.configured ? h("button", { class: "link", onclick: change("webhook.set", () => ({ url: "", key: "" })) }, "Remove webhook") : null,
    h("h2", {}, "Security"),
    receipts.length ? h("ul", { class: "receipts" }, receipts.map((r) => h("li", {}, `${fmtTime(r.at)} · ${r.event} with ${r.method}`))) : null,
    h("button", { class: "danger", onclick: async (e) => {
      await busy(e.target, () => ctx.client.call("lock"));
      statusScreen(ctx, "Rubi is locked.");
    } }, "Lock Rubi now"),
  );
}

main().catch((e) => fatal(friendly(e)));
// Opening a new link while the panel is already open only changes the fragment.
window.addEventListener("hashchange", () => main().catch((e) => fatal(friendly(e))));
