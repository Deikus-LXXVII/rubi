// Carries panel requests to Rubi through public Nostr relays (see internal/relay in the Rubi repository).
// The requests are already end-to-end encrypted; relays only forward them. Rubi and the panel use several
// relays at once, and the first copy of an answer wins.

import { schnorr, utils } from "../vendor/noble-secp256k1/secp256k1.js";

export const DEFAULT_RELAYS = [
  "wss://nos.lol",
  "wss://relay.primal.net",
  "wss://relay.snort.social",
  "wss://nostr.mom",
  "wss://relay.nostr.net",
  "wss://nostr.oxtr.dev",
];

const KIND = 21777;
const CHUNK = 30000;
const MAX_PARTS = 64;

const hex = {
  enc: (b) => Array.from(b, (x) => x.toString(16).padStart(2, "0")).join(""),
  dec: (s) => Uint8Array.from(s.match(/../g).map((x) => parseInt(x, 16))),
};

async function sha256(bytes) {
  return new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
}

function randomId() {
  const b = crypto.getRandomValues(new Uint8Array(12));
  return btoa(String.fromCharCode(...b)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export class RelayTransport {
  constructor(rubiPub, relays) {
    this.rubi = rubiPub;
    this.relays = relays && relays.length ? relays : DEFAULT_RELAYS;
    this.secret = utils.randomSecretKey();
    this.pub = hex.enc(schnorr.getPublicKey(this.secret));
    this.sockets = new Set();
    this.waits = new Map(); // message id -> {parts, have, status, resolve}
    this.seen = new Set();
    this.ready = null;
  }

  // start opens all relays and resolves once one of them is subscribed.
  start() {
    if (this.ready) return this.ready;
    this.ready = new Promise((resolve, reject) => {
      let pending = this.relays.length;
      const timer = setTimeout(() => reject(new Error("no relay reachable")), 10000);
      for (const url of this.relays) {
        this.open(url, () => {
          clearTimeout(timer);
          resolve();
        }, () => {
          if (--pending === 0) {
            clearTimeout(timer);
            reject(new Error("no relay reachable"));
          }
        });
      }
    });
    this.ready.catch(() => { this.ready = null; });
    return this.ready;
  }

  open(url, onReady, onFail) {
    let ws;
    try {
      ws = new WebSocket(url);
    } catch {
      onFail();
      return;
    }
    let opened = false;
    ws.onopen = () => {
      opened = true;
      const filter = { kinds: [KIND], authors: [this.rubi], "#p": [this.pub], since: Math.floor(Date.now() / 1000) - 60 };
      ws.send(JSON.stringify(["REQ", "rubi", filter]));
      this.sockets.add(ws);
      // Give the relay a moment to register the subscription before the first request goes out.
      setTimeout(onReady, 250);
    };
    ws.onerror = () => { if (!opened) onFail(); };
    ws.onclose = () => {
      this.sockets.delete(ws);
      if (!opened) return;
      setTimeout(() => this.open(url, () => {}, () => {}), 3000); // reconnect while the page is open
    };
    ws.onmessage = (m) => this.receive(m.data);
  }

  async receive(data) {
    let msg;
    try {
      msg = JSON.parse(data);
    } catch {
      return;
    }
    if (msg[0] !== "EVENT" || !msg[2]) return;
    const e = msg[2];
    if (e.kind !== KIND || e.pubkey !== this.rubi || this.seen.has(e.id)) return;
    this.seen.add(e.id);
    if (!(await this.verify(e))) return;
    let p;
    try {
      p = JSON.parse(e.content);
    } catch {
      return;
    }
    const w = this.waits.get(p.r);
    if (!w || p.v !== 1 || p.n < 1 || p.n > MAX_PARTS || p.i < 0 || p.i >= p.n) return;
    if (!w.parts) w.parts = new Array(p.n).fill(null);
    if (w.parts.length !== p.n || w.parts[p.i] !== null) return;
    w.parts[p.i] = p.d;
    w.have += 1;
    if (p.s) w.status = p.s;
    if (w.have === p.n) {
      this.waits.delete(p.r);
      w.resolve({ status: w.status || 0, body: w.parts.join("") });
    }
  }

  serialize(e) {
    return new TextEncoder().encode(JSON.stringify([0, e.pubkey, e.created_at, e.kind, e.tags, e.content]));
  }

  async verify(e) {
    try {
      const id = await sha256(this.serialize(e));
      if (hex.enc(id) !== e.id) return false;
      return await schnorr.verifyAsync(hex.dec(e.sig), id, hex.dec(e.pubkey));
    } catch {
      return false;
    }
  }

  async sign(content) {
    const e = { pubkey: this.pub, created_at: Math.floor(Date.now() / 1000), kind: KIND, tags: [["p", this.rubi]], content };
    const id = await sha256(this.serialize(e));
    e.id = hex.enc(id);
    e.sig = hex.enc(await schnorr.signAsync(id, this.secret));
    return e;
  }

  // post sends one request body and resolves with Rubi's answer {status, body}.
  async post(body, timeoutMs = 30000) {
    await this.start();
    if (body.length > CHUNK * MAX_PARTS) throw new Error("request too large");
    const r = randomId();
    const n = Math.max(1, Math.ceil(body.length / CHUNK));
    const events = [];
    for (let i = 0; i < n; i++) {
      events.push(await this.sign(JSON.stringify({ v: 1, r, i, n, d: body.slice(i * CHUNK, (i + 1) * CHUNK) })));
    }
    const answer = new Promise((resolve) => this.waits.set(r, { have: 0, resolve }));
    const send = () => {
      for (const ws of this.sockets) {
        if (ws.readyState !== WebSocket.OPEN) continue;
        for (const e of events) ws.send(JSON.stringify(["EVENT", e]));
      }
    };
    send();
    // Relays that connect a bit later still get the request.
    const resend = setTimeout(send, 2500);
    let timer;
    const timeout = new Promise((_, reject) => { timer = setTimeout(() => reject(new Error("no answer through the relays")), timeoutMs); });
    try {
      return await Promise.race([answer, timeout]);
    } finally {
      clearTimeout(timer);
      clearTimeout(resend);
      this.waits.delete(r);
    }
  }
}
