// Talks to one Rubi instance over the end-to-end encrypted channel.

import { b64u, fromUtf8, openResponse, rand, sealRequest, utf8 } from "./crypto.js";

export class UserError extends Error {}

// parseLink reads the fragment of a panel link: #v=1&e=…&k=…&a=…&p=…|t=…
export function parseLink(hash) {
  const q = new URLSearchParams(hash.replace(/^#/, ""));
  if (q.get("v") !== "1" || !q.get("e") || !q.get("k")) return null;
  const e = q.get("e").replace(/\/+$/, "");
  if (!/^https:\/\//.test(e) && !/^http:\/\/(127\.0\.0\.1|localhost)(:\d+)?$/.test(e)) return null;
  let k;
  try {
    k = b64u.dec(q.get("k"));
  } catch {
    return null;
  }
  if (k.length !== 64) return null;
  return { e, k: q.get("k"), x25519: k.slice(0, 32), a: q.get("a") || "", p: q.get("p") || "", t: q.get("t") || "" };
}

export class RubiClient {
  constructor(link) {
    this.link = link;
  }

  async call(op, args) {
    const rid = b64u.enc(rand(16));
    const env = { op, ts: Math.floor(Date.now() / 1000), rid };
    if (this.link.t) env.ticket = this.link.t;
    if (args !== undefined) env.args = args;
    const { request, respKey } = await sealRequest(this.link.x25519, "/v1/rpc", utf8(JSON.stringify(env)));

    let resp;
    try {
      resp = await fetch(this.link.e + "/v1/rpc", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(request),
        cache: "no-store",
        credentials: "omit",
        referrerPolicy: "no-referrer",
      });
    } catch {
      throw new UserError("Can't reach your Rubi. It may have restarted; ask your agent for a new link.");
    }
    if (!resp.ok) throw new UserError(`Your Rubi rejected the request (HTTP ${resp.status}). Ask your agent for a new link.`);

    let reply;
    try {
      reply = JSON.parse(fromUtf8(await openResponse(respKey, rid, await resp.json())));
    } catch {
      throw new UserError("This answer did not come from your Rubi. Do not continue, and tell your agent.");
    }
    if (!reply.ok) throw new UserError(reply.error || "Request failed.");
    return reply.result;
  }
}
