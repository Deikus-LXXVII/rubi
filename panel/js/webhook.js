// Reading a Grok Bot routine's webhook out of whatever the user pasted: the "POST to" and "header" (or
// "key") fields in either order, the whole Webhook section, a curl command, with or without "Bearer ".
// Rubi checks the result again (internal/core/routinehook.go); this is for clear messages on the spot.

export const ROUTINE_HOST = "api2.cursor.sh";
const ROUTINE_PATH = /^\/automations\/webhook\/[^/]+$/;
const MAX_PASTE = 8192;
const URLS = /\bhttps?:\/\/[^\s"'<>`\\]+/gi;
const ROUTINE_KEYS = /\bcrsr_[A-Za-z0-9_-]{8,500}/g;

// isRoutineURL: https://api2.cursor.sh/automations/webhook/<routine id>, nothing else on that host.
export function isRoutineURL(u) {
  return u.protocol === "https:" && u.hostname === ROUTINE_HOST && !u.port && !u.username && !u.password &&
    ROUTINE_PATH.test(u.pathname) && !u.search && !u.hash;
}

function urlsIn(t) {
  const out = [];
  for (const m of t.match(URLS) || []) {
    try {
      out.push(new URL(m.replace(/[.,;:)\]}]+$/, "")));
    } catch { /* not a URL after all */ }
  }
  return out;
}

const distinct = (xs) => [...new Set(xs)];

// parseWebhook returns {url, key, routine, error}. routine: both are a Grok Bot routine's. With strict
// (an agent's Connect link) only a routine is accepted; otherwise any https receiver with a key token.
export function parseWebhook(text, { strict = false } = {}) {
  const t = String(text || "");
  const res = { url: "", key: "", routine: false, error: "" };
  if (t.length > MAX_PASTE) {
    res.error = "That's a lot of text. Copy only the routine's Webhook section (\u201cPOST to\u201d and \u201cheader\u201d).";
    return res;
  }
  const urls = urlsIn(t);
  const routines = distinct(urls.filter(isRoutineURL).map((u) => u.href));
  if (routines.length > 1) {
    res.error = "That's two different routines' addresses. Paste the fields of one routine only.";
    return res;
  }
  if (routines.length === 1) {
    res.url = routines[0];
  } else if (urls.length) {
    const other = urls[0];
    const local = other.hostname === "127.0.0.1" || other.hostname === "localhost";
    if (strict && other.hostname === ROUTINE_HOST) {
      res.error = `Copy \u201cPOST to\u201d exactly as the routine shows it: https://${ROUTINE_HOST}/automations/webhook/\u2026`;
      return res;
    }
    if (strict) {
      res.error = `\u201c${other.host}\u201d isn't a Grok Bot routine. Copy \u201cPOST to\u201d from the routine itself; it starts with https://${ROUTINE_HOST}/.`;
      return res;
    }
    if (other.username || other.password) {
      res.error = "That address has a user name in it. Copy the address again.";
      return res;
    }
    if (other.protocol !== "https:" && !local) {
      res.error = "The address must start with https://.";
      return res;
    }
    res.url = other.href;
  }

  // The key, from what's left once the addresses are taken out.
  const rest = t.replace(URLS, " ");
  const keys = distinct(rest.match(ROUTINE_KEYS) || []);
  if (keys.length > 1) {
    res.error = "That's two different keys. Paste the fields of one routine only.";
    return res;
  }
  if (keys.length === 1) {
    res.key = keys[0];
  } else if (!strict) {
    res.key = (rest.match(/\bBearer\s+([^\s"']+)/i) || [])[1] ||
      rest.split(/\s+/).map((w) => w.replace(/^["']|["']$/g, ""))
        .filter((w) => w.length >= 16 && !/^(authorization|bearer|key|header|post|to|content-type|application\/json)\b/i.test(w))[0] || "";
  }
  if (res.key && /[^\x21-\x7e]/.test(res.key)) res.key = "";
  res.routine = !!res.url && routines.length === 1 && res.key.startsWith("crsr_");
  return res;
}

// missingHint says what to paste next, or "" when both are there.
export function missingHint(w) {
  if (w.error) return w.error;
  if (!w.url && !w.key) return "Paste the routine's \u201cPOST to\u201d address and its \u201cheader\u201d.";
  if (!w.url) return "Got the key. Now copy \u201cPOST to\u201d and paste it here too.";
  if (!w.key) return "Got the address. Now copy the \u201cheader\u201d (or \u201ckey\u201d) and paste it here too.";
  return "";
}
