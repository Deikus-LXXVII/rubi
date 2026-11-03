// A pairing code from Rubi Home, kept for a few minutes. Rubi Home's window opens the panel with
// #home-code=<code>; that code has no secret (the PIN in Rubi Home's window is the secret, typed here), so
// nothing in a URL, in history or in this storage can pair a computer by itself.
//
// Pure helpers with no page code, so a Go test can run them under node (internal/repocheck/homecode_node_test.go).

export const HOME_CODE_KEY = "rubi.homecode.v1";
export const HOME_CODE_MS = 15 * 60 * 1000;

const b64url = (b) => btoa(String.fromCharCode(...b)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

function storage() {
  try {
    return globalThis.localStorage || null; // the getter itself throws in some private modes
  } catch {
    return null;
  }
}

// decodeHomeCode reads "rubi-home:" + base64url(JSON). It returns {relay, bundle, name, hasSecret}, or null.
export function decodeHomeCode(code) {
  try {
    const s = String(code || "").replace(/\s+/g, "");
    if (!/^rubi-home:[A-Za-z0-9_-]{20,8000}$/.test(s)) return null;
    const b64 = s.slice("rubi-home:".length).replace(/-/g, "+").replace(/_/g, "/");
    const bin = atob(b64 + "=".repeat((4 - (b64.length % 4)) % 4));
    const v = JSON.parse(new TextDecoder().decode(Uint8Array.from(bin, (c) => c.charCodeAt(0))));
    if (!v || typeof v.n !== "string" || typeof v.k !== "string" || !v.n || !v.k) return null;
    return { relay: v.n, bundle: v.k, name: typeof v.m === "string" ? v.m : "", hasSecret: typeof v.c === "string" && v.c !== "" };
  } catch {
    return null;
  }
}

// homeFingerprint is base64url(sha256(the bundle string's bytes)[:6]): the same as Rubi Home's window
// (home.Helper.Fingerprint) and the approval's preview (core PairDevice) compute from the same text.
export async function homeFingerprint(bundle) {
  const d = new Uint8Array(await globalThis.crypto.subtle.digest("SHA-256", new TextEncoder().encode(bundle)));
  return b64url(d.slice(0, 6));
}

// keepHomeCode stores {code, until} and says whether it did. A code with a secret is never stored: a secret
// belongs in no URL and no storage. A bad code, or storage that throws, gives false.
export function keepHomeCode(code, now = Date.now()) {
  try {
    const d = decodeHomeCode(code);
    const s = storage();
    if (!d || d.hasSecret || !s) return false;
    s.setItem(HOME_CODE_KEY, JSON.stringify({ code: String(code).replace(/\s+/g, ""), until: now + HOME_CODE_MS }));
    return true;
  } catch {
    return false;
  }
}

export function clearHomeCode() {
  try {
    storage()?.removeItem(HOME_CODE_KEY);
  } catch {
    /* nothing to clear */
  }
}

// waitingHomeCode is the stored code, or "" when there is none or it has expired. An expired or damaged item
// is deleted.
export function waitingHomeCode(now = Date.now()) {
  try {
    const s = storage();
    const raw = s?.getItem(HOME_CODE_KEY);
    if (raw == null) return "";
    const v = JSON.parse(raw);
    if (v && typeof v.code === "string" && decodeHomeCode(v.code) && Number(v.until) > now) return v.code;
  } catch {
    /* damaged: deleted below */
  }
  clearHomeCode();
  return "";
}

// ---- the Mac app's download link ----
// release.yml uploads rubi-home-<tag>-macos-universal.zip (one app for Apple silicon and Intel; scripts/build-home-app.sh
// makes the name) to the tag's GitHub release. The site's /releases/latest.json (deploy/pages/latest.jq, run by
// pages.yml) names the newest tag, and carries `mac_app` only when that release really has the file.

export const HOME_FEED = "https://rubi-panel.com/releases/latest.json";
export const HOME_RELEASES = "https://github.com/Deikus-LXXVII/rubi/releases";
const RELEASE_TAG = /^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$/; // the same rule as install-home.sh and the build script

// homeFeedVersion is the release tag a feed names, or "" when it is not a plain tag.
export function homeFeedVersion(feed) {
  const v = feed && typeof feed.version === "string" ? feed.version : "";
  return RELEASE_TAG.test(v) ? v : "";
}

// homeMacAppURL is the download address of the app in the newest release, or "" when the feed does not say that
// release has it (the caller then links to the releases page instead of a file that may be missing).
export function homeMacAppURL(feed) {
  const v = homeFeedVersion(feed);
  const file = `rubi-home-${v}-macos-universal.zip`;
  if (!v || !feed || feed.mac_app !== file) return "";
  return `${HOME_RELEASES}/download/${v}/${file}`;
}
