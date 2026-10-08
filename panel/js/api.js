// Talks to one Rubi instance over the end-to-end encrypted channel.

import { b64u, fromUtf8, openResponse, rand, sealRequest, utf8 } from "./crypto.js";
import { RelayTransport } from "./relay.js";

export class UserError extends Error {}

// parseLink reads the fragment of a panel link: #v=1&k=…&a=…&p=…|t=… plus a transport: n=<relay key>
// (and optionally r=<relays>) for the relay transport, and/or e=<url> for a direct HTTPS endpoint.
export function parseLink(hash) {
  const q = new URLSearchParams(hash.replace(/^#/, ""));
  if (q.get("v") !== "1" || (!q.get("e") && !q.get("n")) || !q.get("k")) return null;
  // Plain http and ws only for a panel that is itself served locally (development): a link on the real
  // panel must not point it at services on the user's own machine.
  const local = /^(127\.0\.0\.1|localhost)$/.test(globalThis.location?.hostname || "");
  const e = (q.get("e") || "").replace(/\/+$/, "");
  if (e && !/^https:\/\//.test(e) && !(local && /^http:\/\/(127\.0\.0\.1|localhost)(:\d+)?$/.test(e))) return null;
  const n = q.get("n") || "";
  if (n && !/^[0-9a-f]{64}$/.test(n)) return null;
  const r = (q.get("r") || "").split(",").filter(Boolean);
  if (r.some((u) => !/^wss:\/\/[^/\s]+\/?$/.test(u) && !(local && /^ws:\/\/(127\.0\.0\.1|localhost)(:\d+)?\/?$/.test(u)))) return null;
  let k;
  try {
    k = b64u.dec(q.get("k"));
  } catch {
    return null;
  }
  if (k.length !== 64) return null;
  return { e, n, r, k: q.get("k"), x25519: k.slice(0, 32), a: q.get("a") || "", p: q.get("p") || "", t: q.get("t") || "" };
}

// One relay connection per Rubi is shared by every client on the page.
const transports = new Map();
function relayFor(link) {
  if (!transports.has(link.n)) transports.set(link.n, new RelayTransport(link.n, link.r));
  return transports.get(link.n);
}

export class RubiClient {
  constructor(link) {
    this.link = link;
  }

  // post sends a sealed request: through relays when the link offers them, else (or if they fail) HTTPS.
  async post(body) {
    if (this.link.n) {
      try {
        const res = await relayFor(this.link).post(body);
        if (res.status !== 200) throw new UserError(`Your Rubi rejected the request (${res.status}). Ask your agent for a new link.`);
        return res.body;
      } catch (err) {
        if (err instanceof UserError || !this.link.e) {
          if (err instanceof UserError) throw err;
          throw new UserError("Can't reach your Rubi. It may be restarting; wait a moment, or ask your agent for a new link.");
        }
      }
    }
    let resp;
    try {
      resp = await fetch(this.link.e + "/v1/rpc", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body,
        cache: "no-store",
        credentials: "omit",
        referrerPolicy: "no-referrer",
      });
    } catch {
      throw new UserError("Can't reach your Rubi. It may have restarted; ask your agent for a new link.");
    }
    if (!resp.ok) throw new UserError(`Your Rubi rejected the request (HTTP ${resp.status}). Ask your agent for a new link.`);
    return resp.text();
  }

  async call(op, args) {
    const rid = b64u.enc(rand(16));
    const env = { op, ts: Math.floor(Date.now() / 1000), rid };
    if (this.link.t) env.ticket = this.link.t;
    if (args !== undefined) env.args = args;
    const { request, respKey } = await sealRequest(this.link.x25519, "/v1/rpc", utf8(JSON.stringify(env)));

    const raw = await this.post(JSON.stringify(request));
    let reply;
    try {
      reply = JSON.parse(fromUtf8(await openResponse(respKey, rid, JSON.parse(raw))));
    } catch {
      throw new UserError("This answer did not come from your Rubi. Do not continue, and tell your agent.");
    }
    if (!reply.ok) throw new UserError(reply.error || "Request failed.");
    return reply.result;
  }
}
