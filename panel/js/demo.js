// A demo of the panel with made-up data: no Rubi, no keys, nothing leaves the page. Opened with #demo
// (a gallery of screens) or #demo=<screen>. It lets people see what approving looks like before they
// install anything, and lets us review every screen while designing.

const now = () => Date.now();
const iso = (ms) => new Date(ms).toISOString();

export const DEMO_HELLO = { instance: "rubi_demo", fingerprint: "nVFu hOVN 4rUy", state: "unlocked", version: "v0.6.0" };

const APPROVALS = {
  send: {
    kind: "icloud-mail.send", summary: "Send email to Anna Petrova: \"Dinner on Friday\"",
    question: "Notify you when a reply arrives?",
    options: [{ key: "send", label: "Send" }, { key: "send_track", label: "Send and notify on reply" }],
    preview: {
      from: "Alex <alex@icloud.com>", to: "Anna Petrova <anna@example.com>", subject: "Dinner on Friday",
      body: "Hi Anna,\n\nAre you free for dinner on Friday at 19:30? I booked a table at Mirabelle, the place on the corner we liked last time.\n\nIf Friday doesn't work, Saturday is fine too.\n\nSee you,\nAlex",
    },
  },
  install: {
    kind: "rubi.plugin.install", summary: "Install Philips Hue",
    question: "Tell your agent when a new version is out?",
    options: [{ key: "install", label: "Install" }, { key: "install_notify", label: "Install and notify me of updates" }],
    preview: {
      plugin: "Philips Hue", about: "Turn your Hue lights on and off, dim them and recall scenes.",
      publisher: "Rubi-Project · key 2F6A-91C3", review: "Reviewed by Rubi-Project", new_version: "v1.0.0",
      can: "See your lights and scenes (No approval); Change lights (No approval)",
      rubi_home: "Uses your Rubi Home computer for: Philips Hue",
    },
  },
  reveal: {
    kind: "icloud-mail.private", summary: "Show 3 private emails to your agent",
    options: [{ key: "show", label: "Show" }],
    preview: { reason: "You asked me to log in to your bank's new app; it sent a code.", emails: "3 (choose which below)" },
    items: [
      { key: "41", label: "bank.example: Your sign-in code", preview: { from: "Bank <no-reply@bank.example>", date: "Today 14:02" } },
      { key: "42", label: "bank.example: New device signed in", preview: { from: "Bank <alerts@bank.example>", date: "Today 14:03" } },
      { key: "43", label: "shop.example: Confirm your email", preview: { from: "Shop <hello@shop.example>", date: "Yesterday" } },
    ],
  },
  door: {
    kind: "apple-home.run_sensitive", summary: "Run the shortcut \"Unlock the front door\"",
    options: [{ key: "run", label: "Run" }],
    preview: { shortcut: "Unlock the front door", input: "(none)", rubi_home: "Mac mini" },
  },
  settings: {
    kind: "rubi.settings", summary: "Add Rubi Home on Mac mini",
    options: [{ key: "apply", label: "Approve" }],
    preview: { "computer (as it calls itself)": "Mac mini", fingerprint: "pX3k-9QaL (rubi-home status shows the same)",
      allows: "control Philips Hue on your home network and run the shortcuts in the folder \"Rubi\"" },
  },
};

function approval(id) {
  const a = APPROVALS[id] || APPROVALS.send;
  return { id, state: "pending", expires_at: iso(now() + 14 * 60 * 1000 + 30 * 1000), ...a };
}

const PLUGINS = [
  { id: "icloud-mail", name: "iCloud Mail", version: "v2.0.0", installed: true, reviewed: true, connected: true, has_config: true,
    accounts: [{ id: "alex@icloud.com", label: "alex@icloud.com", default: true }, { id: "work@icloud.com", label: "work@icloud.com" }],
    previous_version: "v1.2.0" },
  { id: "gmail", name: "Gmail", version: "v1.0.0", installed: true, reviewed: true, connected: true, has_config: true,
    accounts: [{ id: "alex.k@gmail.com", label: "alex.k@gmail.com", default: true }] },
  { id: "steam", name: "Steam", version: "v1.0.0", installed: true, reviewed: true, connected: true, has_config: true,
    update_available: "v1.1.0", accounts: [{ id: "76561198000000000", label: "Alex", default: true }] },
  { id: "presence", name: "Location", version: "v1.0.0", installed: true, reviewed: true, connected: false, has_config: true },
  { id: "hue", name: "Philips Hue", version: "v1.0.0", installed: false, reviewed: true, description: "Lights and scenes through Rubi Home." },
  { id: "telegram", name: "Unofficial Telegram", version: "v1.0.0", installed: false, reviewed: true, description: "A bot or your personal account, with approvals." },
  { id: "apple-home", name: "Apple Home", version: "v1.0.0", installed: false, reviewed: true, description: "Your Shortcuts, run by your agent with your say." },
];

export class DemoClient {
  constructor() {
    this.link = { a: "demo", k: "demo" };
    this.demo = true;
  }

  async call(op, args = {}) {
    await new Promise((r) => setTimeout(r, 260 + Math.random() * 240)); // feels like a real round trip
    switch (op) {
      case "hello":
        return DEMO_HELLO;
      case "status":
        return { state: "unlocked", integrity: { status: "verified", version: "v0.6.0" },
          receipts: [{ at: iso(now() - 3600e3), event: "unlocked", method: "passkey" }, { at: iso(now() - 86400e3 * 2), event: "paired", method: "passkey" }] };
      case "approval.get":
        return { approval: approval(args.approval_id), approvers: [{ credential_id: "demo" }], password: true, agent_notified: true,
          challenges: {} };
      case "approval.challenge":
        return { challenge: "" };
      case "approval.decide": {
        const a = approval(args.approval_id);
        return { ...a, state: args.approve ? "executed" : "denied", result: args.option === "send_track" ? { tracking: {} } : {} };
      }
      case "unlock.keys":
        return { instance: "rubi_demo", wraps: [{ kind: "passkey" }, { kind: "password" }] };
      case "store.list":
        return { plugins: PLUGINS };
      case "policy.get":
        return { levels: ["none", "chat", "strong"], actions: [
          { integration: "iCloud Mail", kind: "icloud-mail.read", title: "Read and search mail", default: "none", level: "none" },
          { integration: "iCloud Mail", kind: "icloud-mail.send", title: "Send email", default: "strong", level: "strong" },
          { integration: "iCloud Mail", kind: "icloud-mail.private", title: "Show a private email", default: "strong", level: "strong", locked: true },
          { integration: "Steam", kind: "steam.watch", title: "Watch for deals", default: "none", level: "chat" },
        ] };
      case "agents.get":
        return { sources: [{ id: "rubi", name: "Rubi itself" }, { id: "icloud-mail", name: "iCloud Mail" }, { id: "steam", name: "Steam" }],
          agents: [{ name: "Mail", host: "routines.grok.example", default: true, subscriptions: ["rubi", "icloud-mail"] },
            { name: "Gamer", host: "routines.grok.example", subscriptions: ["steam"] }] };
      case "devices.get":
        return { devices: [{ id: "d1", name: "Mac mini", paired: iso(now() - 86400e3) }] };
      case "device.check":
        return { online: true, version: "v0.6.0" };
      case "updates.list":
        return { updates: [
          { id: "rubi", name: "Rubi", current: "v0.6.0", available: false, notify: true },
          { id: "steam", name: "Steam", current: "v1.0.0", latest: "v1.1.0", available: true, notify: true },
          { id: "icloud-mail", name: "iCloud Mail", current: "v2.0.0", available: false, notify: false },
        ] };
      case "integration.catalog":
        return { integrations: [{ id: "telegram", name: "Unofficial Telegram", connected: false, has_config: true,
          needs: "A bot token from @BotFather, or your phone number for your personal account.",
          fields: [{ key: "kind", label: "What to connect", type: "choice", required: true,
            options: [{ key: "bot", label: "A bot (token from @BotFather)" }, { key: "personal", label: "My personal account" }] }],
          secrets: [], egress: ["api.telegram.org:443"] }] };
      case "integration.setup":
        if (!args.fields?._step) {
          return { need_more: { step: "s1", message: "Telegram sent a code to your Telegram app. Enter it here.",
            fields: [{ key: "code", label: "Login code", type: "number", required: true }] } };
        }
        return { approval_id: "settings", ticket: "demo", account: "@alex" };
      case "plugin.config.get":
        return { id: args.id, name: "iCloud Mail", account: "alex@icloud.com",
          accounts: [{ id: "alex@icloud.com", label: "alex@icloud.com", default: true }, { id: "work@icloud.com", label: "work@icloud.com" }],
          fields: [
            { key: "folder_access", label: "Folders your agent can see", type: "choice", per_account: true,
              options: [{ key: "all", label: "All folders" }, { key: "selected", label: "Only the folders checked below" }] },
            { key: "folders", label: "Allowed folders", type: "list", per_account: true,
              options: [{ key: "INBOX", label: "INBOX" }, { key: "Archive", label: "Archive" }, { key: "Receipts", label: "Receipts" }] },
            { key: "hide_codes", label: "Hide sign-in codes, one-time passwords and confirmation links", type: "bool" },
            { key: "hide_sign_in_alerts", label: "Hide sign-in and security alerts", type: "bool",
              help: "Off by default, so your agent can warn you about new sign-ins." },
            { key: "hidden_senders", label: "Hidden senders", type: "list", help: "Addresses or domains, one per line." },
          ],
          values: { folder_access: "all", folders: ["INBOX"], hide_codes: true, hide_sign_in_alerts: false, hidden_senders: ["bank.example"] } };
      case "plugin.config.set":
      case "plugin.install":
      case "plugin.update":
      case "plugin.remove":
      case "plugin.rollback":
      case "integration.disconnect":
      case "device.pair":
      case "device.remove":
      case "policy.set":
      case "updates.plugin":
      case "updates.rubi":
        return { approval_id: "settings", ticket: "demo" };
      case "updates.notify":
      case "agent.subscribe":
        return { subscriptions: args.sources || [] };
      case "webhook.test":
      case "lock":
        return {};
    }
    throw new Error(`The demo doesn't do "${op}".`);
  }
}

// The screens the gallery offers, in the order a new user meets them.
export const DEMO_SCREENS = [
  ["pair", "Set up Rubi"],
  ["unlock", "Unlock"],
  ["send", "Approve an email"],
  ["install", "Install a plugin"],
  ["reveal", "Choose what to show"],
  ["door", "A sensitive shortcut"],
  ["settings", "Settings"],
  ["store", "Plugin store"],
  ["config", "Plugin settings"],
  ["setup", "Connect with a code"],
  ["home", "Add a home computer"],
  ["bots", "Grok Bot connections"],
  ["updates", "Updates"],
  ["sent", "Sent"],
  ["declined", "Declined"],
  ["failed", "It didn't work"],
  ["status", "Status"],
  ["error", "An error"],
];
