// Cryptography for the Rubi panel. Mirrors internal/e2e and internal/panelclient in the Go code;
// see docs/design/panel-protocol.md. Uses WebCrypto only, plus hash-wasm for Argon2id.

const subtle = globalThis.crypto.subtle;
const te = new TextEncoder();
const td = new TextDecoder();

export const utf8 = (s) => te.encode(s);
export const fromUtf8 = (b) => td.decode(b);
export const rand = (n) => globalThis.crypto.getRandomValues(new Uint8Array(n));

export function concat(...parts) {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}

// base64url without padding (the protocol's encoding).
export const b64u = {
  enc(bytes) {
    const b = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
    let s = "";
    for (const x of b) s += String.fromCharCode(x);
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  },
  dec(str) {
    const s = String(str).replace(/-/g, "+").replace(/_/g, "/");
    const pad = s.length % 4 ? "=".repeat(4 - (s.length % 4)) : "";
    return Uint8Array.from(atob(s + pad), (c) => c.charCodeAt(0));
  },
};

// Standard base64 with padding (how Go encodes []byte in JSON).
export function b64std(bytes) {
  let s = "";
  for (const x of new Uint8Array(bytes)) s += String.fromCharCode(x);
  return btoa(s);
}

export async function hkdf(ikm, salt, info, length) {
  const key = await subtle.importKey("raw", ikm, "HKDF", false, ["deriveBits"]);
  const bits = await subtle.deriveBits({ name: "HKDF", hash: "SHA-256", salt, info: utf8(info) }, key, length * 8);
  return new Uint8Array(bits);
}

async function gcmKey(raw, usage) {
  return subtle.importKey("raw", raw, "AES-GCM", false, [usage]);
}

export async function sealGcm(key, plain, ad) {
  const nonce = rand(12);
  const ct = await subtle.encrypt({ name: "AES-GCM", iv: nonce, additionalData: utf8(ad) }, await gcmKey(key, "encrypt"), plain);
  return { nonce, ct: new Uint8Array(ct) };
}

export async function openGcm(key, nonce, ct, ad) {
  const p = await subtle.decrypt({ name: "AES-GCM", iv: nonce, additionalData: utf8(ad) }, await gcmKey(key, "decrypt"), ct);
  return new Uint8Array(p);
}

export async function sha256(bytes) {
  return new Uint8Array(await subtle.digest("SHA-256", bytes));
}

export async function hmacSha256(key, msg) {
  const k = await subtle.importKey("raw", key, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return new Uint8Array(await subtle.sign("HMAC", k, msg));
}

export async function supportsX25519() {
  try {
    await subtle.generateKey({ name: "X25519" }, false, ["deriveBits"]);
    return true;
  } catch {
    return false;
  }
}

// ---- end-to-end channel ----

export async function sealRequest(instancePub, path, plain) {
  const eph = await subtle.generateKey({ name: "X25519" }, true, ["deriveBits"]);
  const peer = await subtle.importKey("raw", instancePub, { name: "X25519" }, false, []);
  const shared = new Uint8Array(await subtle.deriveBits({ name: "X25519", public: peer }, eph.privateKey, 256));
  const epk = new Uint8Array(await subtle.exportKey("raw", eph.publicKey));
  const okm = await hkdf(shared, concat(epk, instancePub), "rubi e2e v1", 64);
  const { nonce, ct } = await sealGcm(okm.slice(0, 32), plain, "rubi-req|v1|" + path);
  return {
    request: { v: 1, epk: b64u.enc(epk), nonce: b64u.enc(nonce), ct: b64u.enc(ct) },
    respKey: okm.slice(32),
  };
}

export async function openResponse(respKey, rid, sealed) {
  return openGcm(respKey, b64u.dec(sealed.nonce), b64u.dec(sealed.ct), "rubi-resp|v1|" + rid);
}

// ---- key wrapping (the DEK never leaves this device unwrapped, except to unlock Rubi) ----

export function newPasswordKdf() {
  return { alg: "argon2id", salt: b64u.enc(rand(16)), time: 3, memory_kib: 64 * 1024, threads: 1 };
}

export async function passwordKek(password, kdf) {
  if (kdf.alg !== "argon2id" || kdf.memory_kib < 19 * 1024) throw new Error("Unsupported password settings.");
  if (!globalThis.hashwasm) throw new Error("Password support failed to load.");
  return globalThis.hashwasm.argon2id({
    password,
    salt: b64u.dec(kdf.salt),
    parallelism: kdf.threads,
    iterations: kdf.time,
    memorySize: kdf.memory_kib,
    hashLength: 32,
    outputType: "binary",
  });
}

export async function prfKek(prfOutput, prfSalt) {
  return hkdf(new Uint8Array(prfOutput), prfSalt, "rubi kek v1", 32);
}

const wrapAD = (instance, id) => `rubi-wrap|v1|${instance}|${id}`;

export async function wrapDek(kek, dek, instance, id) {
  const { nonce, ct } = await sealGcm(kek, dek, wrapAD(instance, id));
  return { nonce: b64u.enc(nonce), wrapped: b64u.enc(ct) };
}

export async function unwrapDek(kek, wrap, instance) {
  return openGcm(kek, b64u.dec(wrap.nonce), b64u.dec(wrap.wrapped), wrapAD(instance, wrap.id));
}

export async function approveKey(passwordKekBytes) {
  return hkdf(passwordKekBytes, new Uint8Array(0), "rubi approve v1", 32);
}
