// Passkeys (WebAuthn). A passkey does two jobs:
//  - unlock: its PRF extension yields a secret that derives the key wrapping Rubi's data key;
//  - approve: it signs a challenge bound to one exact action.

import { b64std, b64u, rand } from "./crypto.js";

export function passkeysAvailable() {
  return typeof window.PublicKeyCredential === "function" && !!navigator.credentials;
}

function rpId() {
  return location.hostname;
}

// create registers a new passkey for this Rubi and tries to get its PRF output right away.
export async function createPasskey({ instance, fingerprint, prfSalt }) {
  const cred = await navigator.credentials.create({
    publicKey: {
      rp: { id: rpId(), name: "Rubi" },
      user: { id: rand(16), name: `Rubi ${fingerprint}`, displayName: `Rubi (${fingerprint})` },
      challenge: rand(32),
      pubKeyCredParams: [
        { type: "public-key", alg: -7 },
        { type: "public-key", alg: -8 },
        { type: "public-key", alg: -257 },
      ],
      authenticatorSelection: { residentKey: "required", userVerification: "required" },
      extensions: { prf: { eval: { first: prfSalt } } },
      timeout: 120000,
    },
  });
  if (!cred) throw new Error("Passkey was not created.");
  const r = cred.response;
  const spki = r.getPublicKey ? r.getPublicKey() : null;
  if (!spki) throw new Error("This browser can't register passkeys for Rubi. Use a password.");
  const ext = cred.getClientExtensionResults?.() || {};
  const credentialId = b64u.enc(cred.rawId);
  let prfOutput = ext.prf?.results?.first || null;
  if (!prfOutput && ext.prf?.enabled) {
    // Some platforms only return PRF results from an assertion.
    prfOutput = (await evalPrf([{ credentialId, prfSalt }]))?.prfOutput || null;
  }
  return {
    credentialId,
    approver: {
      credential_id: credentialId,
      public_key: b64std(spki), // Go []byte JSON encoding
      alg: r.getPublicKeyAlgorithm(),
      label: "Passkey",
    },
    prfOutput: prfOutput ? new Uint8Array(prfOutput) : null,
  };
}

// evalPrf asks for one of the given passkeys and returns which one answered plus its PRF output.
export async function evalPrf(entries) {
  const evalByCredential = {};
  for (const e of entries) evalByCredential[e.credentialId] = { first: e.prfSalt };
  const cred = await navigator.credentials.get({
    publicKey: {
      rpId: rpId(),
      challenge: rand(32),
      allowCredentials: entries.map((e) => ({ type: "public-key", id: b64u.dec(e.credentialId) })),
      userVerification: "required",
      extensions: { prf: { evalByCredential } },
      timeout: 120000,
    },
  });
  if (!cred) return null;
  const out = cred.getClientExtensionResults?.().prf?.results?.first;
  if (!out) throw new Error("This passkey can't unlock Rubi on this device. Use your password.");
  return { credentialId: b64u.enc(cred.rawId), prfOutput: new Uint8Array(out) };
}

// signChallenge produces the assertion Rubi verifies for an approval.
export async function signChallenge(challenge, credentialIds) {
  const cred = await navigator.credentials.get({
    publicKey: {
      rpId: rpId(),
      challenge,
      allowCredentials: credentialIds.map((id) => ({ type: "public-key", id: b64u.dec(id) })),
      userVerification: "required",
      timeout: 120000,
    },
  });
  if (!cred) throw new Error("Approval was cancelled.");
  const r = cred.response;
  return {
    type: "passkey",
    credential_id: b64u.enc(cred.rawId),
    client_data_json: b64u.enc(r.clientDataJSON),
    authenticator_data: b64u.enc(r.authenticatorData),
    signature: b64u.enc(r.signature),
  };
}
