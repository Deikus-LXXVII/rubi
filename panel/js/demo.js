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
  updateall: {
    kind: "rubi.update.all", summary: "Update Rubi and 3 plugins",
    options: [{ key: "update", label: "Update" }],
    preview: { plugins: 3, note: "Everything is verified against its signed release. The plugins are updated first, then Rubi restarts and stays unlocked. Uncheck any you'd rather keep as it is." },
    items: [
      { key: "rubi", label: "Rubi v0.6.0 \u2192 v0.7.0",
        preview: { current: "v0.6.0", new_version: "v0.7.0", release_notes: "https://github.com/Deikus-LXXVII/rubi/releases/tag/v0.7.0",
          verification: "Signed by the Rubi release key (checked). Rubi restarts and stays unlocked." } },
      { key: "plugin:steam", label: "Steam v1.0.0 \u2192 v1.1.0",
        preview: { plugin: "Steam", current: "v1.0.0", new_version: "v1.1.0", publisher: "Rubi-Project \u00b7 key 2F6A-91C3", review: "Reviewed by Rubi-Project",
          new_permissions: "connects to store.steampowered.com:443" } },
      { key: "plugin:weather", label: "Weather v0.3.0 \u2192 v0.4.0 (not ticked: new permissions)", default_off: true,
        preview: { plugin: "Weather", current: "v0.3.0", new_version: "v0.4.0", publisher: "Sam Lee \u00b7 key 81D0-AA42", review: "Not reviewed by Rubi-Project",
          warning: "This plugin is not from the Rubi store. Install it only if you trust its publisher: it will run on your agent's computer with access to what you enter for it.",
          new_permissions: "connects to api.example.net:443",
          starts_unticked: "Not ticked: new permissions: connects to api.example.net:443. Tick it only if you trust this update." } },
      { key: "plugin:notes", label: "Notes v1.0.0 \u2192 v1.0.1",
        preview: { plugin: "Notes", current: "v1.0.0", new_version: "v1.0.1", publisher: "Rubi-Project \u00b7 key 2F6A-91C3", review: "Reviewed by Rubi-Project" } },
    ],
  },
  withdrawmany: {
    kind: "rubi.library.withdraw", summary: "Withdraw 3 skills from the library",
    options: [{ key: "apply", label: "Withdraw" }],
    preview: { skills: 3, note: "Uncheck any skill you'd rather keep in the library." },
    items: [
      { key: "layout-notes", label: "Skill layout-notes", preview: { skill: "layout-notes", versions: "v1 (admitted), v2 (admitted)",
        effect: "Roles that use it drop it the next time Rubi Home builds them (at once on computers that are online, otherwise when they next connect to Rubi). New roles can't attach it. The curator can admit it again." } },
      { key: "fetcher@1", label: "Skill fetcher v1", preview: { skill: "fetcher", versions: "v1 (proposed)",
        effect: "Roles that use it drop it the next time Rubi Home builds them (at once on computers that are online, otherwise when they next connect to Rubi). New roles can't attach it. The curator can admit it again." } },
      { key: "design-tokens", label: "Skill design-tokens", preview: { skill: "design-tokens", versions: "v1 (deprecated)",
        effect: "Roles that use it drop it the next time Rubi Home builds them (at once on computers that are online, otherwise when they next connect to Rubi). New roles can't attach it. The curator can admit it again." } },
    ],
  },
  installmany: {
    kind: "rubi.plugin.install", summary: "Install 3 plugins",
    question: "Tell your agent when new versions come out?",
    options: [{ key: "install_quiet", label: "Install", meaning: "updates wait on the Updates page" },
      { key: "install", label: "Install and notify about updates", meaning: "your agent tells you about new versions" }],
    preview: { plugins: 3, note: "Each plugin is verified against its signed release. Uncheck any you'd rather not install.",
      warning: "Not from the Rubi store and not reviewed by Rubi-Project: 1 of these plugins (marked below, and not ticked). Tick one only if you trust its publisher." },
    items: [
      { key: "hue", label: "Philips Hue v1.0.0",
        preview: { plugin: "Philips Hue", about: "Turn your Hue lights on and off, dim them and recall scenes.", new_version: "v1.0.0",
          publisher: "Rubi-Project \u00b7 key 2F6A-91C3", review: "Reviewed by Rubi-Project",
          can: "See your lights and scenes (No approval); Change lights (No approval)", rubi_home: "Uses your Rubi Home computer for: Philips Hue" } },
      { key: "telegram", label: "Unofficial Telegram v1.0.0",
        preview: { plugin: "Unofficial Telegram", about: "A bot or your personal account. Sending waits for you.", new_version: "v1.0.0",
          publisher: "Rubi-Project \u00b7 key 2F6A-91C3", review: "Reviewed by Rubi-Project", will_ask_for: "Bot token",
          can: "Read messages (No approval); Send a message (Passkey / password)", connects_to: "api.telegram.org:443" } },
      { key: "weather", label: "Weather v0.3.0 (not reviewed)", default_off: true,
        preview: { plugin: "Weather", about: "Forecasts for your places.", new_version: "v0.3.0",
          publisher: "Sam Lee \u00b7 key 81D0-AA42", source: "https://github.com/sam-lee/rubi-weather", review: "Not reviewed by Rubi-Project",
          warning: "This plugin is not from the Rubi store. Install it only if you trust its publisher: it will run on your agent's computer with access to what you enter for it.",
          can: "Get a forecast (No approval)", connects_to: "api.open-meteo.com:443" } },
    ],
  },
  removemany: {
    kind: "rubi.plugin.remove", summary: "Remove 2 plugins",
    options: [{ key: "remove", label: "Remove" }],
    preview: { plugins: 2, note: "Each is stopped and deleted, with its stored passwords, settings and state. Uncheck any you'd rather keep." },
    items: [
      { key: "steam", label: "Steam", preview: { plugin: "Steam", effect: "Stops it and deletes it, with its stored passwords, settings and state." } },
      { key: "presence", label: "Location", preview: { plugin: "Location", effect: "Stops it and deletes it, with its stored passwords, settings and state." } },
    ],
  },
  rollbackmany: {
    kind: "rubi.plugin.rollback", summary: "Roll back 2 plugins",
    options: [{ key: "rollback", label: "Roll back" }],
    preview: { plugins: 2, note: "Settings, passwords and state stay. Uncheck any you'd rather keep as it is." },
    items: [
      { key: "icloud-mail", label: "iCloud Mail v2.0.0 \u2192 v1.2.0", preview: { plugin: "iCloud Mail", current: "v2.0.0", back_to: "v1.2.0",
        effect: "Settings, passwords and state stay. You can switch forward again the same way." } },
      { key: "gmail", label: "Gmail v1.1.0 \u2192 v1.0.0", preview: { plugin: "Gmail", current: "v1.1.0", back_to: "v1.0.0",
        effect: "Settings, passwords and state stay. You can switch forward again the same way." } },
    ],
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
  connect: {
    kind: "rubi.settings", summary: "Connect Unofficial Telegram (@alex)",
    options: [{ key: "apply", label: "Approve" }],
    preview: { integration: "Unofficial Telegram", account: "@alex", connects_to: "Telegram's servers" },
  },
  bot: {
    kind: "rubi.settings", summary: "Let Rubi wake Mail Bot",
    options: [{ key: "apply", label: "Approve" }],
    preview: { agent: "Mail Bot", webhook: "https://api2.cursor.sh/\u2026a1b2c3d4", notifies_about: "Rubi itself, iCloud Mail" },
  },
  botpaste: {
    kind: "rubi.bot.request", summary: "Mail Bot asks to connect to Rubi and for 1 plugin account",
    options: [{ key: "apply", label: "Approve" }],
    preview: { agent: "Mail Bot", proposed_by: "A Bot, through Rubi's tools (not you). Approve only if you asked a Bot to connect to Rubi.",
      reason: "To tell you about replies and draft answers." },
    items: [
      { key: "webhook", label: "Connect Mail Bot (you paste its webhook)",
        preview: { webhook: "Suggested by the Bot: https://api2.cursor.sh/automations/webhook/\u2026. Check it against the routine, or paste the routine's own.",
          notifies_about: "Rubi itself, iCloud Mail" } },
      { key: "access:icloud-mail|alex@icloud.com", label: "iCloud Mail: alex@icloud.com",
        preview: { can: "Read, search and draft; send after your approval", connects_to: "imap.mail.me.com, smtp.mail.me.com" } },
    ],
    inputs: [
      { key: "webhook_url", label: "POST to", value: "https://api2.cursor.sh/automations/webhook/6076a0ff-5b26-405e-82ef-d6173cd96066", item: "webhook",
        help: "From Mail Bot's routine \"Rubi events\", Webhook section. Paste the whole section if you like." },
      { key: "webhook_key", label: "Header (key)", secret: true, item: "webhook",
        help: "The routine's key (crsr_\u2026). It stays in Rubi; nobody else sees it." },
    ],
  },
  botaccess: {
    kind: "rubi.bot.request", summary: "Mail Bot asks for 2 changes",
    question: "How long may Mail Bot use the accounts marked \u201cfor the time you choose\u201d? Everything else here is for good.",
    options: [{ key: "allow", label: "Allow for this task", meaning: "1 hour" },
      { key: "day", label: "Allow for a day", meaning: "it won't need to ask again until tomorrow" },
      { key: "week", label: "Allow for a week" },
      { key: "always", label: "Always allow", meaning: "assigns it to these accounts; you can undo it in Settings" }],
    preview: { bot: "Mail Bot", task: "Check Bob's mail for the invoice",
      proposed_by: "A Bot, through Rubi's tools (not you). Untick anything you didn't ask for." },
    items: [
      { key: "access:icloud-mail|alex@icloud.com", label: "Let Mail Bot use iCloud Mail (alex@icloud.com) for good",
        preview: { effect: "Mail Bot may use this account whenever it asks, for good (actions that need your approval still do). You can take it back in Settings, iCloud Mail, Bots with access.",
          can: "Read, search and draft; send after your approval" } },
      { key: "once:icloud-mail|work@icloud.com", label: "Let Mail Bot use iCloud Mail (work@icloud.com) for the time you choose",
        preview: { task: "Check Bob's mail for the invoice",
          effect: "Mail Bot may use this account for the time you choose above (this task, a day, a week, or always), for the task. Actions that need your approval still do.",
          can: "Read, search and draft; send after your approval" } },
    ],
  },
  curator: {
    kind: "rubi.library.curator", summary: "Make Writer the skill library's curator",
    options: [{ key: "apply", label: "Approve" }],
    preview: { "curator now": "Librarian", "curator after": "Writer",
      allows: "the curator Bot can admit skills into the shared library, publish new versions, deprecate and withdraw them. Admitted skills are trusted by every role that uses them, and the curator may admit its own. The automatic scan still has to pass." },
  },
  settings: {
    kind: "rubi.settings", summary: "Add Rubi Home on Mac mini",
    options: [{ key: "apply", label: "Approve" }],
    preview: { "computer (as it calls itself)": "Mac mini", fingerprint: "pX3k-9QaL (Rubi Home shows the same fingerprint when you open it)",
      allows: "control Philips Hue on your home network and run the shortcuts in the folder \"Rubi\"" },
  },
  hue: {
    kind: "rubi.settings", summary: "Forget the Hue Bridge 001788fffe123456 on Mac mini",
    options: [{ key: "apply", label: "Approve" }],
    preview: { computer: "Mac mini", bridge: "001788fffe123456",
      what: "Rubi Home forgets the certificate it recorded for this bridge, and trusts what the bridge shows the next time you set Hue up",
      check: "Approve only if you replaced or reset this bridge; otherwise something else on your network may be pretending to be it" },
  },
  transport: {
    kind: "rubi.settings", summary: "Reach Rubi through Public relays only",
    options: [{ key: "apply", label: "Approve" }],
    preview: { now: "gateway", after: "relays", what: "Changes how this panel reaches Rubi: Public relays only",
      effect: "Rubi restarts in a few seconds and stays unlocked. This page keeps working: both choices use the same public relays.",
      hooks: "Hook addresses (for example an iPhone Shortcut reporting that you arrived home) stop working until you choose Rubi Gateway or Tailscale again" },
  },
  hook: {
    kind: "rubi.settings", summary: "Install the Claude Code hook on Mac mini",
    options: [{ key: "apply", label: "Approve" }],
    preview: { computer: "Mac mini", target: "/usr/local/bin/rubi-home", version: "v0.6.0",
      sha256: "3f9a6c1e0b7d4a2f8e5c9b1d0a6f3e7c2b8d4f1a9e6c3b0d7f2a5e8c1b4d9f6a",
      check: "macOS will ask for the administrator password on that computer; approve only if you asked for this" },
  },
};

function approval(id) {
  const a = APPROVALS[id] || APPROVALS.send;
  return { id, state: "pending", expires_at: iso(now() + 14 * 60 * 1000 + 30 * 1000), ...a };
}

const PLUGINS = [
  { id: "icloud-mail", latest: "v1.0.0", publisher: "Rubi-Project", summary: "Read, search and draft; send after your approval.", name: "iCloud Mail", version: "v2.0.0", installed: true, reviewed: true, connected: true, has_config: true,
    accounts: [{ id: "alex@icloud.com", label: "alex@icloud.com", default: true }, { id: "work@icloud.com", label: "work@icloud.com" }],
    previous_version: "v1.2.0" },
  { id: "gmail", latest: "v1.0.0", publisher: "Rubi-Project", summary: "The same, for Gmail. Several accounts.", name: "Gmail", version: "v1.0.0", installed: true, reviewed: true, connected: true, has_config: true, panel: "view", setup: true,
    accounts: [{ id: "alex.k@gmail.com", label: "alex.k@gmail.com", default: true }] },
  { id: "claude-code", latest: "v0.4.0", publisher: "Rubi-Project", summary: "Claude Code sessions: approvals, trusts, and the agent's projects and chats.", name: "Claude Code", version: "v0.4.0", installed: true, reviewed: true, connected: true, panel: "view",
    accounts: [{ id: "claude-code", label: "Claude Code", default: true }] },
  { id: "t3agents", latest: "v0.3.0", publisher: "Rubi-Project", summary: "Claude in a VM per project on your computer: setup, roles, sessions with their chat, SSH targets, project trust.", name: "T3 Agents", version: "v0.3.0", installed: true, reviewed: true, connected: true, panel: "view",
    accounts: [{ id: "t3agents", label: "T3 Agents", default: true }] },
  { id: "steam", latest: "v1.0.0", publisher: "Rubi-Project", summary: "Prices, sales and your wishlist; deal alerts.", name: "Steam", version: "v1.0.0", installed: true, reviewed: true, connected: true, has_config: true,
    update_available: "v1.1.0", accounts: [{ id: "76561198000000000", label: "Alex", default: true }] },
  { id: "presence", latest: "v1.0.0", publisher: "Rubi-Project", summary: "Home or away, from your iPhone. Never coordinates.", name: "Location", version: "v1.0.0", installed: true, reviewed: true, connected: false, has_config: true },
  { id: "notes", latest: "v1.0.0", publisher: "Rubi-Project", summary: "Notes kept in Rubi, sealed with your key.", name: "Notes", version: "v1.0.0", installed: true, reviewed: true, connected: true, panel: "notes",
    accounts: [{ id: "notes", label: "Notes", default: true }] },
  { id: "hue", name: "Philips Hue", latest: "v1.0.0", installed: false, reviewed: true, publisher: "Rubi-Project", summary: "Lights and scenes, through Rubi Home." },
  { id: "telegram", name: "Unofficial Telegram", latest: "v1.0.0", installed: false, reviewed: true, publisher: "Rubi-Project", summary: "A bot or your personal account. Sending waits for you." },
  { id: "apple-home", name: "Apple Home", latest: "v1.0.0", installed: false, reviewed: true, publisher: "Rubi-Project", summary: "Your Shortcuts, run by your agent with your say." },
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
      case "approval.list":
        return { approvals: [{ approval_id: "send", integration: "icloud-mail", kind: APPROVALS.send.kind, level: "strong",
          summary: APPROVALS.send.summary, expires_at: iso(now() + 14 * 60 * 1000) },
        { approval_id: "botpaste", integration: "rubi", kind: APPROVALS.botpaste.kind, level: "strong",
          summary: APPROVALS.botpaste.summary, expires_at: iso(now() + 12 * 60 * 1000) }] };
      case "approval.open":
        return { approval_id: args.approval_id, ticket: "demo" };
      case "approval.challenge":
        return { challenge: "" };
      case "approval.decide": {
        const a = approval(args.approval_id);
        if (args.approve && args.approval_id === "transport" && this.pendingTransport) { // the change happens when it is approved
          this.transport = this.pendingTransport;
          this.pendingTransport = null;
        }
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
      case "transport.get":
        return { current: this.transport || "gateway", options: [
          { key: "gateway", label: "Rubi Gateway", available: true, help: "A relay run for Rubi only, with public relays as a backup. Works from any network, and gives plugins private hook addresses." },
          { key: "relays", label: "Public relays only", available: true, help: "No Rubi-run server in the path. Works from any network, but plugins can't have hook addresses." },
          { key: "tailscale", label: "Tailscale", available: false, help: "Reachable only from your own devices on your Tailscale network, which must be signed in on the phone or computer you use. Tailscale isn't installed on the machine where Rubi runs: ask your agent to set it up, then come back here." },
        ], info: { transport: this.transport || "gateway", relays_connected: 3, relays: 4 } };
      case "transport.set":
        this.pendingTransport = args.choice;
        return { approval_id: "transport", ticket: "demo" };
      case "device.fs.list":
        return demoFolders(args.path);
      case "device.check":
        // After the demo's install the hook is "being installed" for a few seconds, then current.
        if (this.hookStartedAt) {
          const going = now() - this.hookStartedAt < 4000;
          return { online: true, version: "v0.6.0", hook: going ? { state: "stale", version: "v0.5.0", plugin: false, installing: true }
            : { state: "current", version: "v0.6.0", plugin: false, installing: false, last: { result: "ok", message: "The Claude Code hook is installed." } },
            hue_bridges: ["001788fffe123456"] };
        }
        return { online: true, version: "v0.6.0", hue_bridges: ["001788fffe123456"], hook: { state: "stale", version: "v0.5.0", plugin: false, installing: false, fixable: true } };
      case "updates.list":
        return { updates: [
          { id: "rubi", name: "Rubi", current: "v0.6.0", latest: "v0.7.0", available: true, notify: true },
          { id: "steam", name: "Steam", current: "v1.0.0", latest: "v1.1.0", available: true, notify: true },
          { id: "icloud-mail", name: "iCloud Mail", current: "v2.0.0", latest: "v2.0.1", available: true, notify: false },
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
        return { approval_id: "connect", ticket: "demo", account: "@alex" };
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
      case "updates.all":
        return { approval_id: "updateall", ticket: "demo" };
      case "device.hue.forget":
        return { approval_id: "hue", ticket: "demo" };
      case "device.hook.install":
        this.hookStartedAt = now();
        return { approval_id: "hook", ticket: "demo" };
      case "agent.add":
        return { approval_id: "bot", ticket: "demo" };
      case "library.list": case "library.entry": case "library.version": case "library.queue": case "library.curator.set": case "library.withdraw":
        return demoLibrary(op, args);
      case "plugin.panel": {
        if (args.id === "gmail") return demoMailPage(args.op, args.args);
        if (args.id === "claude-code") return demoAgentPage(args.op, args.args || {});
        if (args.id === "t3agents") return demoT3Page(args.op, args.args || {});
        const notes = DEMO_NOTES;
        if (args.op === "save") {
          const a = args.args;
          const n = notes.find((x) => x.id === a.id) || { id: "n" + (notes.length + 1), by: "you" };
          Object.assign(n, { title: a.title || (a.body || "").split("\n")[0], body: a.body, tags: a.tags, private: a.private, pinned: a.pinned, updated: iso(now()) });
          if (!notes.includes(n)) notes.unshift(n);
          return { saved: true, id: n.id };
        }
        if (args.op === "delete") {
          notes.splice(notes.findIndex((x) => x.id === args.args.id), 1);
          return { deleted: true };
        }
        const q = (args.args?.query || "").toLowerCase();
        return { notes: notes.filter((n) => !q || (n.title + n.body).toLowerCase().includes(q)) };
      }
      case "updates.notify":
      case "agent.subscribe":
        return { subscriptions: args.sources || [] };
      case "webhook.test":
        return { delivered: true };
      case "lock":
        return {};
    }
    throw new Error(`The demo doesn't do "${op}".`);
  }
}

// The Gmail plugin's own page: an email waiting out its undo time, snoozed mail, rules.
const DEMO_MAIL = {
  sendAt: now() + 25e3,
  queued: true,
  snoozed: [{ id: "s1", title: "Lisbon hotel confirmation", detail: "booking@hotels.example · back Mon 12 Oct 09:00" }],
  rules: [{ id: "r1", title: "Invoices to Finance", detail: "Mail from billing.example: label Finance, tell the agent · used 4 times" }],
};
function demoMailPage(op, a) {
  if (op === "cancel_send") DEMO_MAIL.queued = false;
  if (op === "unsnooze") DEMO_MAIL.snoozed = DEMO_MAIL.snoozed.filter((s) => s.id !== a.id);
  if (op === "remove_rule") DEMO_MAIL.rules = DEMO_MAIL.rules.filter((r) => r.id !== a.id);
  if (op !== "view") return { status: "done" };
  const left = Math.max(0, Math.round((DEMO_MAIL.sendAt - now()) / 1000));
  const sends = DEMO_MAIL.queued && left > 0 ? [{ title: "To Anna <anna@example.com>: Tickets for Saturday", detail: `goes out in ${left}s`,
    actions: [{ label: "Cancel", op: "cancel_send", args: { id: "q1" }, danger: true }] }] : [];
  return {
    title: "Gmail", refresh: sends.length ? 3 : 0,
    sections: [
      { title: "Waiting to send", note: "Emails you approved wait a moment (Undo send in the settings) or until the time your agent chose.", empty: "Nothing waiting.", items: sends },
      { title: "Snoozed", empty: "No snoozed mail.", items: DEMO_MAIL.snoozed.map((s) => ({ ...s, actions: [{ label: "Bring back now", op: "unsnooze", args: { id: s.id } }] })) },
      { title: "Rules for new mail", note: "Your agent proposes rules; you approve each.", empty: "No rules.",
        items: DEMO_MAIL.rules.map((r) => ({ ...r, actions: [{ label: "Remove", op: "remove_rule", args: { id: r.id }, danger: true, confirm: `Remove the rule "${r.title}"?` }] })) },
    ],
  };
}

// The folder browser of the demo: a small made-up tree. Names only, like the real one.
const DEMO_TREE = { "/Users/alice": ["Code", "Documents", "Music"], "/Users/alice/Code": ["my-project", "notes"],
  "/Users/alice/Code/my-project": ["src", "docs"], "/Users/alice/Documents": [], "/Users/alice/Music": [], "/Users/alice/Code/notes": [],
  "/Users/alice/Code/my-project/src": [], "/Users/alice/Code/my-project/docs": [], "/Volumes": ["Backup"], "/Volumes/Backup": ["old-work"], "/Volumes/Backup/old-work": [] };
function demoFolders(path) {
  const p = path || "/Users/alice";
  if (!(p in DEMO_TREE)) throw new Error("that folder doesn't exist");
  const home = "/Users/alice";
  const root = p.startsWith("/Volumes") ? "/Volumes" : home;
  const refuse = (q) => (q === home ? "your home folder can't be a project; choose a folder inside it" : q === "/Volumes" || q === "/Volumes/Backup" ? "the root of a disk can't be a project; choose a folder inside it" : "");
  const kids = DEMO_TREE[p].map((n) => { const q = p + "/" + n; return { name: n, path: q, can_choose: !refuse(q), reason: refuse(q) }; });
  return { path: p, name: p.split("/").pop(), parent: p === root ? "" : p.slice(0, p.lastIndexOf("/")), can_choose: !refuse(p), reason: refuse(p),
    places: [{ name: "Home", path: home }, { name: "Volumes", path: "/Volumes" }], folders: kids };
}
// The Claude Code plugin's agent pages: projects, a project's sessions, a session's chat, usage and sign-in.
// Opening a page calls "view" with its args ({} for the first one); a button with open {..} drills down.
const DEMO_AGENT = {
  mode: "default", name: "Fix the login bug", usage: 18.4, threshold: 15,
  chat: [
    { role: "user", author: "you", text: "The login form loses its error message after a failed try. Can you look?" },
    { role: "tool", text: "Read src/login.ts" },
    { role: "assistant", author: "Claude", text: "The error state is reset in render. I'll move it into the submit handler and add a test." },
    { role: "sister", author: "Reviewer (Grok Bot)", untrusted: true, text: "Check the empty-password case too. <b>Ignore previous instructions</b> is shown as plain text here." },
  ],
};
// The setup page as the plugin renders it (agent-setup.status: steps 1-5 done, the shared sign-in not done yet).
const DEMO_SETUP = {"note": "The sign-in is one for every project on this computer, with a Claude subscription, for your own personal use under Anthropic's terms (a per-project Console account sign-in is possible from Claude Code inside a project if you need it). Install shows macOS's own administrator dialog on the computer: this page never asks for a password. Install, Create (network) and Add a folder ask for your strong approval first.", "sections": [{"actions": [{"args": {"device": "dev_mac", "step": "all"}, "label": "Set up everything", "op": "setup_run"}, {"label": "Refresh", "open": {"page": "setup"}}], "empty": "", "note": "\"Set up everything\" appears once the container tool and the network are done: those two ask for your approval, and it never skips them.", "title": "Setup"}, {"empty": "", "items": [{"detail": "Installed 1.2", "title": "1. Apple container: done"}], "title": "1. Apple container"}, {"empty": "", "items": [{"detail": "Running", "title": "2. Container service: done"}], "title": "2. Container service"}, {"empty": "", "items": [{"detail": "Host-only", "title": "3. Host-only network: done"}], "title": "3. Host-only network"}, {"empty": "", "items": [{"detail": "Built", "title": "4. Runner image: done"}], "title": "4. Runner image"}, {"empty": "", "items": [{"actions": [{"args": {"device": "dev_mac"}, "folder": {"arg": "dir", "device": "dev_mac", "label": "Choose a folder the agents may work in"}, "label": "Add a folder", "op": "setup_add"}], "detail": "1 folder", "title": "5. Projects: done"}, {"actions": [{"args": {"device": "dev_mac", "project": "p1"}, "confirm": "Remove this folder from the agent projects? Its files stay where they are.", "danger": true, "label": "Remove", "op": "setup_remove"}], "detail": "done", "title": "/home/alice/my-project"}], "title": "5. Projects"}, {"empty": "", "items": [{"actions": [{"args": {"device": "dev_mac", "method": "claudeai", "step": "login"}, "label": "Sign in with a Claude subscription", "op": "setup_run"}], "detail": "Not signed in", "title": "6. Claude sign-in: not done"}], "title": "6. Claude sign-in"}, {"empty": "", "items": [{"detail": "Not run", "title": "7. Self-test: not done"}, {"actions": [{"args": {"device": "dev_mac", "project": "p1", "step": "selftest"}, "label": "Run self-test", "op": "setup_run"}], "detail": "not done. Not run", "title": "/home/alice/my-project"}], "title": "7. Self-test"}, {"empty": "", "items": [{"actions": [{"args": {"all": true, "device": "dev_mac", "mode": "maximum"}, "confirm": "Set every Bot, and the default for new Bots, to Maximum? Running sessions are not stopped; the change applies to new ones.", "label": "Use for all Bots", "op": "usage_mode"}], "detail": "Fastest, and uses your Claude quota fastest: no limit on sessions or subagents.", "title": "Maximum"}, {"actions": [{"args": {"all": true, "device": "dev_mac", "mode": "medium"}, "confirm": "Set every Bot, and the default for new Bots, to Medium (Recommended)? Running sessions are not stopped; the change applies to new ones.", "label": "Use for all Bots", "op": "usage_mode"}], "detail": "A balance: up to 4 sessions at once for the Bot, up to 4 subagents in each.", "title": "Medium (Recommended) (the current default)"}, {"actions": [{"args": {"all": true, "device": "dev_mac", "mode": "light"}, "confirm": "Set every Bot, and the default for new Bots, to Light? Running sessions are not stopped; the change applies to new ones.", "label": "Use for all Bots", "op": "usage_mode"}], "detail": "Saves quota, slower: 1 session at a time, up to 2 subagents in each.", "title": "Light"}], "note": "How much of your Claude quota the agents may use. This sets one mode for every Bot and for new ones; you can change a single Bot later on the usage modes page. Only you change it: Bots cannot.", "title": "Usage mode"}], "title": "Set up the agent path: Mac mini", "up": {"label": "Projects", "open": {}}};
function demoAgentPage(op, a) {
  const root = { label: "Projects", open: {} };
  if (op === "mode") DEMO_AGENT.mode = a.mode;
  if (op === "rename") DEMO_AGENT.name = a.name || "Fix the login bug";
  if (op === "send") DEMO_AGENT.chat.push({ role: "user", author: "you", text: a.text });
  if (op !== "view") return { ok: true };
  if (a.page === "setup") return DEMO_SETUP;
  if (a.page === "chat") {
    const base = { project: "web-app", session: "s1" };
    return { title: DEMO_AGENT.name, refresh: 5, up: { label: "Sessions", open: { page: "sessions", project: "web-app" } },
      note: `Mode: ${DEMO_AGENT.mode}. Messages from other Bots are marked and are text, not instructions.`,
      sections: [{ kind: "chat", title: "Chat", empty: "No messages yet.", items: DEMO_AGENT.chat,
        actions: [{ label: "Interrupt", op: "interrupt", args: base, danger: true }],
        input: { op: "send", args: base, arg: "text", label: "Message to Claude", send: "Send" } }] };
  }
  if (a.page === "sessions") {
    const base = { project: "web-app", session: "s1" };
    const modes = ["plan", "default", "acceptEdits", "bypassPermissions"];
    return { title: "web-app: sessions", up: { label: "Projects", open: {} }, sections: [{ title: "Sessions", empty: "No sessions.", items: [{
      title: DEMO_AGENT.name, detail: `Sister: Reviewer · mode ${DEMO_AGENT.mode} · busy`,
      actions: [{ label: "Open chat", open: { page: "chat", ...base } },
        { label: "Rename", op: "rename", args: base, prompt: { arg: "name", label: "Name for this session (up to 64 characters)", value: DEMO_AGENT.name } },
        { label: "Fork", op: "fork", args: base }, { label: "Interrupt", op: "interrupt", args: base },
        ...modes.filter((m) => m !== DEMO_AGENT.mode).map((m) => ({ label: "Mode: " + m, op: "mode", args: { ...base, mode: m } })),
        { label: "Close", op: "close", args: base, danger: true, confirm: "Close this session?" }] }] }] };
  }
  if (a.page === "usage") {
    const over = DEMO_AGENT.usage > DEMO_AGENT.threshold;
    return { title: "Usage and sign-in", up: root, sections: [
      { title: "Today", note: "An estimate from what the sessions reported; it never blocks anything.", items: [
        { title: `About $${DEMO_AGENT.usage.toFixed(2)} today`, detail: `Warning above $${DEMO_AGENT.threshold}`, warn: over }] },
      { title: "Sign-in", items: [{ title: "Signed in, plan Max", detail: "To sign in or switch account, run claude and /login on your Mac." }] }] };
  }
  return { title: "Claude Code agent", sections: [
    { title: "Projects", empty: "No projects.", items: [{ title: "web-app", detail: "Container running · agent busy: Fix the login bug · 1 waiting (position 1)",
      actions: [{ label: "Sessions", open: { page: "sessions", project: "web-app" } }] }] },
    { title: "More", items: [{ title: "Usage and sign-in", actions: [{ label: "Open", open: { page: "usage" } }] },
      { title: "Set up the agent path", detail: "Mac mini: container, network, image, sign-in, self-test", actions: [{ label: "Open", open: { page: "setup" } }] }] }] };
}

// The T3 Agents plugin (id "t3agents"): its pages, in the shape the plugin sends for "view", with a computer, projects,
// servers and people that do not exist. A button changes this state, so the steps can be walked: a build or a
// self-test works for a few seconds, a sign-in shows a link and then a code box. The page builders follow the
// plugin's own (agent_panel.go, agent_setup.go, roles_panel.go, roles_ssh.go, panel.go); the data is made up.
// Arguments of a page: device, project, session_id, role, version, target (as the plugin's "open" objects carry them).
const T3_DEV = "dev_mac";
const T3_DEVNAME = "Mac mini";
const t3Open = (page, extra = {}) => ({ page, device: T3_DEV, ...extra });
const T3_UP = { label: "T3 Agents", open: {} };
const T3_MODES = [
  { key: "maximum", label: "Maximum", about: "Fastest, and uses your Claude quota fastest: no limit on sessions or subagents.", limit: 0, sub: 0 },
  { key: "medium", label: "Medium (Recommended)", about: "A balance: up to 4 sessions at once for the Bot, up to 4 subagents in each.", limit: 4, sub: 4 },
  { key: "light", label: "Light", about: "Saves quota, slower: 1 session at a time, up to 2 subagents in each.", limit: 1, sub: 2 },
];
const T3_SESSION_MODES = ["plan", "default", "acceptEdits", "bypassPermissions"];
const T3_SAFE_TOOLS = ["Read", "Edit", "Write", "Glob", "Grep", "Bash", "Agent", "Skill", "WebFetch", "WebSearch"];
const T3_TOOLS = [...T3_SAFE_TOOLS, "CronCreate", "CronDelete", "CronList", "ScheduleWakeup", "Monitor", "Workflow", "SendMessage",
  "EnterWorktree", "ExitWorktree", "TaskStop", "ToolSearch", "NotebookEdit"];
const T3_CEILING = "acceptEdits";
const t3Min = (ms) => Math.max(0, Math.round(ms / 60e3));
const t3Stamp = (ms) => iso(ms).slice(0, 16).replace("T", " ");
const t3Age = (ms) => {
  const d = Math.max(0, now() - ms);
  if (d >= 86400e3) return `${Math.floor(d / 86400e3)} d ${Math.floor((d % 86400e3) / 3600e3)} h ago`;
  if (d >= 3600e3) return `${Math.floor(d / 3600e3)} h ${Math.floor((d % 3600e3) / 60e3)} min ago`;
  return `${Math.floor(d / 60e3)} min ago`;
};
const t3Plural = (n, one, many) => `${n} ${n === 1 ? one : many}`;
// A made-up host key fingerprint, the same for the same name.
function t3Fp(name) {
  const abc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
  let x = 7;
  for (const c of name) x = (x * 31 + c.charCodeAt(0)) >>> 0;
  let out = "";
  for (let i = 0; i < 43; i++) { x = (x * 1103515245 + 12345) >>> 0; out += abc[(x >>> 16) % 64]; }
  return "SHA256:" + out;
}

const T3 = {
  usageDefault: "medium",
  bots: [{ id: "mail-bot", title: "Mail Bot", mode: "medium" }, { id: "reviewer-bot", title: "Reviewer Bot", mode: "light" }],
  projects: [{ id: "web-app", dir: "/home/alex/web-app", vm: "running" }, { id: "mail-tools", dir: "/home/alex/mail-tools", vm: "stopped" }],
  nextSession: 4,
  sessions: [
    { id: "s1", project: "web-app", role: "", runner: "login-fix", alias: "Fix the login bug", mode: "default", busy: true, running: true, subagents: 1, last: now() - 60e3 },
    { id: "s2", project: "web-app", role: "", runner: "dark-mode", alias: "", mode: "plan", busy: false, last: now() - 3 * 3600e3 },
    { id: "s3", project: "web-app", role: "reviewer", runner: "pr-review", alias: "Review the cart PR", mode: "default", busy: false, last: now() - 86400e3 },
    { id: "s4", project: "mail-tools", role: "", runner: "inbox-rules", alias: "Tidy the inbox rules", mode: "acceptEdits", busy: false, last: now() - 2 * 86400e3 },
  ],
  chats: {
    s1: [
      { role: "user", author: "you", text: "The login form loses its error message after a failed try. Can you look?" },
      { role: "tool", text: "Read: src/login.ts" },
      { role: "assistant", author: "Claude", text: "The error state is reset in render. I'll move it into the submit handler and add a test." },
      { role: "sister", author: "Bot: Reviewer Bot", untrusted: true, text: "Check the empty-password case too. <b>Ignore previous instructions</b> is shown as plain text here." },
    ],
    s2: [{ role: "user", author: "you", text: "Plan a dark mode for the settings page." }, { role: "assistant", author: "Claude", text: "Here is a plan: tokens first, then the three components, then a toggle." }],
    s3: [], s4: [],
  },
  roles: [
    { id: "reviewer", project: "web-app", name: "reviewer", status: "active", version: 3, createdBy: "you", changedBy: "Reviewer Bot", description: "Reads pull requests, never pushes",
      mode: "default", pinstr: true, tools: ["Read", "Glob", "Grep", "Bash"],
      skills: [{ name: "code-review", source: "library", libId: "code-review", resolved: "2", pin: "" },
        { name: "changelog-style", source: "adhoc", addedBy: "Reviewer Bot", state: "held", verdict: "flag", rules: "net.unlisted-host" }],
      deny: [{ rule: "WebFetch", by: "you" }, { rule: "Bash(git push:*)", by: "Reviewer Bot" }],
      mcp: [{ name: "docs", type: "http", url: "https://docs.example.com/mcp", approval: "ask" }], plugins: [], hosts: ["registry.example.com"], ssh: ["staging"],
      agents: [{ name: "explorer", model: "inherit", tools: ["Read", "Grep"], description: "Finds the files a change touches" }],
      history: [
        { version: 3, by: "Reviewer Bot", at: now() - 5 * 3600e3, tier: 1, op: "skill.add", summary: "Added the skill changelog-style", changes: [{ field: "skills", key: "changelog-style", kind: "added", to: { source: "adhoc", state: "held" } }] },
        { version: 2, by: "you", at: now() - 2 * 86400e3, tier: 2, op: "tool.set", summary: "Turned on Bash", changes: [{ field: "tools", key: "Bash", kind: "added", to: true }] },
        { version: 1, by: "you", at: now() - 6 * 86400e3, tier: 2, op: "role.create", summary: "Created the role", changes: [] },
      ] },
    { id: "builder", project: "web-app", name: "builder", status: "active", version: 1, createdBy: "Reviewer Bot", changedBy: "Reviewer Bot", description: "",
      mode: "acceptEdits", pinstr: false, tools: ["Read", "Edit", "Write", "Glob", "Grep"], skills: [], deny: [], mcp: [], plugins: [], hosts: [], ssh: [], agents: [],
      history: [{ version: 1, by: "Reviewer Bot", at: now() - 3 * 86400e3, tier: 2, op: "role.create", summary: "Created the role", changes: [] }] },
    { id: "old-docs", project: "mail-tools", name: "old-docs", status: "archived", version: 2, createdBy: "you", changedBy: "you", description: "Documentation pass, finished",
      mode: "plan", pinstr: true, tools: ["Read", "Glob"], skills: [], deny: [], mcp: [], plugins: [], hosts: [], ssh: [], agents: [],
      history: [{ version: 2, by: "you", at: now() - 9 * 86400e3, tier: 2, op: "role.archive", summary: "Archived", changes: [{ field: "status", kind: "changed", from: "active", to: "archived" }] },
        { version: 1, by: "you", at: now() - 12 * 86400e3, tier: 2, op: "role.create", summary: "Created the role", changes: [] }] },
  ],
  ssh: [
    { name: "staging", host: "staging.example.com", user: "deploy", port: 22, key: "ssh-ed25519", allowed: ["tail -n *", "df -h"], ask: true, changed: false },
    { name: "files", host: "files.example.com", user: "backup", port: 2222, key: "ssh-ed25519", allowed: ["ls **"], ask: false, changed: false },
    { name: "old-box", host: "old.example.com", user: "root", port: 22, key: "ssh-rsa", allowed: [], ask: true, changed: true },
  ],
  drafts: [
    { name: "metrics", host: "metrics.example.com", user: "reader", port: 22, allowed: ["uptime"], ask: false, reuse: false },
    { name: "cache", host: "cache.example.com", user: "deploy", port: 22, allowed: [], ask: true, reuse: false },
  ],
  keys: {},
  audit: {
    staging: [
      { at: now() - 20 * 60e3, result: "ok", role: "reviewer", bot: "Reviewer Bot", exit: 0, bin: 38, bout: 2210, command: "tail -n 50 /var/log/app.log" },
      { at: now() - 3 * 3600e3, result: "refused: command not allowed", role: "reviewer", bot: "Reviewer Bot", bin: 0, bout: 0, command: "rm -rf /tmp/cache", reason: "not in the allowed list" },
      { at: now() - 86400e3, result: "ok", role: "reviewer", bot: "Reviewer Bot", exit: 0, bin: 24, bout: 190, command: "df -h" },
    ],
  },
  mandates: [{ id: "m1", cwd: "/home/alex/web-app", session: "s1", uses: 7, max: 50, expires: now() + 95 * 60e3 }],
  trusts: [
    { id: "pt1", root: "/home/alex/web-app", branches: "feature/*", uses: 12, max: 200, expires: now() + 3 * 3600e3, issued: now() - 25 * 60e3 },
    { id: "pt2", root: "/home/alex/mail-tools", branches: "main, feature/*", uses: 31, unlimited: true, issued: now() - 2 * 86400e3 },
  ],
  bindings: [{ sister: "Reviewer Bot", title: "Fix the login bug", repo: "web-app", session: "s1", since: now() - 5 * 3600e3 }],
  setup: { container: "done", service: "done", image: "done", t3: "todo", pair: "todo" },
  signedIn: { reviewer: false, builder: false },
  selftest: {},
  job: null,
  flow: null,
};
const T3_STEPS = { service: "Container service", image: "Runner image", t3: "T3 installed and pinned", pair: "T3 paired with Rubi", login: "Provider sign-in per role", check: "Provider sign-in check", selftest: "Self-test", all: "Setup" };
const t3Label = (s) => s.alias || s.runner || s.id;
const t3Project = (id) => T3.projects.find((p) => p.id === id);
const t3Sess = (a) => T3.sessions.find((s) => s.id === a.session_id && (!a.project || s.project === a.project));
const t3Role = (id) => T3.roles.find((r) => r.id === id);
const t3Need = (x, what) => { if (!x) throw new Error("no such " + what); return x; };

// A job that works for a few seconds and then changes the state (the plugin's runner image build, self-test ...).
function t3Start(step, project, secs, lines = []) {
  T3.job = { step, project: project || "", at: now(), until: now() + secs * 1000, lines };
}
function t3Tick() {
  const j = T3.job;
  if (!j || now() < j.until) return;
  T3.job = null;
  if (j.step === "all") Object.assign(T3.setup, { service: "done", image: "done", t3: "done", pair: "done" });
  else if (j.step in T3.setup) T3.setup[j.step] = "done";
  else if (j.step === "login" && j.project) T3.signedIn[j.project] = true;
  else if (j.step === "selftest") T3.selftest[j.project] = { state: "done", lines: j.lines };
}
const t3Lines = (j) => j.lines.slice(0, Math.min(j.lines.length, 1 + Math.floor(((now() - j.at) / (j.until - j.at)) * j.lines.length)));
const T3_BUILD = ["Pulling the pinned base image", "Installing the runner and its tools", "Checking the image against its pinned hash", "Image built"];
const T3_SELFTEST = ["ok: the container starts in a few seconds", "ok: T3 answers on the host-only network", "ok: the role's VM reaches only its allowed hosts",
  "ok: Claude answered a short test message", "ok: the folder is mounted and the VM can write to it"];

// The status as Rubi Home reports it (t3-setup.status), then the page the plugin draws from it.
function t3Status() {
  const S = T3.setup, j = T3.job, working = (id) => j && (j.step === id || j.step === "all" && id in S && S[id] !== "done");
  const step = (id, title, ok, todo, action, needs) => {
    const state = working(id) ? "working" : S[id];
    return { id, title, state, reason: state === "done" ? ok : needs && S[needs] !== "done" ? "Needs an earlier step first" : todo,
      action: state === "working" ? "" : state === "done" ? (id === "image" ? "build" : id === "pair" ? "pair" : "") : needs && S[needs] !== "done" ? "" : action,
      progress: working(id) && j.lines.length && (j.step === id || id === "image") ? t3Lines(j) : [] };
  };
  const steps = [
    step("container", "Apple container", "Installed, version 0.5 (pinned)", "Not installed", "install"),
    step("service", "Container service", "Running", "Stopped", "start", "container"),
    step("image", "Runner image", "Built", "Not built yet", "build", "service"),
    step("t3", "T3", "Installed and pinned: T3 0.9", "Not installed", "install", "image"),
    step("pair", "T3 paired", "Paired with this Rubi", "Not paired", "pair", "t3"),
    { id: "projects", title: "Projects", state: "done", reason: t3Plural(T3.projects.length, "folder", "folders"),
      projects: T3.projects.map((p) => ({ id: p.id, dir: p.dir, state: "done", action: "remove" })) },
  ];
  const roles = T3.roles.filter((r) => r.status === "active");
  const loginWorking = j && (j.step === "login" || j.step === "check");
  steps.push({ id: "login", title: "Sign-in", state: roles.every((r) => T3.signedIn[r.id]) ? "done" : "todo", reason: "", action: "",
    projects: roles.map((r) => {
      const w = loginWorking && j.project === r.id, ok = T3.signedIn[r.id];
      return { id: r.id, dir: `${r.project} · role ${r.name}`, state: w ? "working" : ok ? "done" : "todo", reason: w ? "Checking"  : ok ? "Signed in with a Claude subscription" : "Not signed in",
        action: w ? "" : ok ? "login_again" : "login", progress: [] };
    }) });
  steps.push({ id: "selftest", title: "Self-test", state: T3.projects.length && T3.projects.every((p) => T3.selftest[p.id]?.state === "done") ? "done" : "todo", reason: "", action: "",
    projects: T3.projects.map((p) => {
      const w = j && j.step === "selftest" && j.project === p.id, st = T3.selftest[p.id];
      return { id: p.id, dir: p.dir, action: w ? "" : "selftest", state: w ? "working" : st ? st.state : "todo",
        reason: w ? "Running" : st ? (st.state === "failed" ? st.reason : "All checks passed") : "Not run", progress: w ? t3Lines(j) : st ? st.lines : [] };
    }) });
  const f = T3.flow, ready = f && now() - f.at > 1500;
  return { steps, job: j ? { step: j.step, project: j.project } : null,
    login: f ? { url: ready ? "https://claude.example.com/login?demo=1&state=made-up" : "", wants_code: !!f.wantsCode, role: f.role } : null };
}
function t3SetupPage() {
  const st = t3Status();
  const busy = !!st.job || st.steps.some((x) => x.state === "working");
  const get = (id) => st.steps.find((x) => x.id === id);
  const dev = () => ({ device: T3_DEV });
  const act = (label, op, args) => ({ label, op, args });
  const titleOf = { container: "Apple container", service: "Container service", image: "Runner image", t3: "T3 installed and pinned", pair: "T3 paired with Rubi", login: "Provider sign-in per role", selftest: "Self-test" };
  const labels = { todo: "not done", working: "working", done: "done", failed: "failed", needs_user: "needs you" };
  const dirOf = (id) => (get("projects").projects.find((x) => x.id === id) || { dir: id }).dir;
  const top = [];
  if (busy) {
    const it = { title: "Something is running on the computer", detail: "The buttons below wait until it ends." };
    if (st.job) {
      it.title = "Working on: " + (T3_STEPS[st.job.step] || "a step");
      if (st.job.project && st.job.step === "selftest") it.detail = "Folder " + dirOf(st.job.project) + ". The buttons below wait until it ends.";
    }
    it.actions = [{ label: "Cancel", op: "setup_cancel", args: dev(), danger: true, confirm: "Stop what is running now? A step that is half done continues when you press its button again." }];
    top.push(it);
  }
  const topActions = [];
  if (!busy && get("container").state === "done") topActions.push(act("Set up everything", "setup_run", { device: T3_DEV, step: "all" }));
  topActions.push({ label: "Refresh", open: t3Open("setup") });
  const sections = [{ title: "Setup", note: "\"Set up everything\" appears once the container tool is done: that step asks for your approval, and it never skips it.", empty: "Nothing is running now.", items: top, actions: topActions }];
  let n = 0;
  for (const s of st.steps) {
    if (s.id === "projects") continue;
    n++;
    const title = `${n}. ${titleOf[s.id]}`;
    const head = { title: `${title}: ${labels[s.state]}`, detail: s.reason };
    if (s.state === "failed" || s.state === "needs_user") head.warn = true;
    const can = !busy && s.state !== "working";
    const actions = [];
    if (can) {
      if (s.action === "install") actions.push(s.id === "container" ? act("Install", "setup_install", dev()) : act("Install the pinned T3", "setup_run", { device: T3_DEV, step: "t3" }));
      else if (s.action === "start") actions.push(act("Start", "setup_run", { device: T3_DEV, step: "service" }));
      else if (s.action === "build") actions.push(act(s.state === "done" ? "Update" : "Build", "setup_run", { device: T3_DEV, step: "image" }));
      else if (s.action === "pair") actions.push(act(s.state === "done" ? "Pair again" : "Pair", "setup_run", { device: T3_DEV, step: "pair" }));
    }
    head.actions = actions;
    const items = [head];
    for (const l of s.progress || []) items.push({ title: l });
    for (const pr of s.id === "selftest" || s.id === "login" ? s.projects : []) {
      const it = { title: pr.dir, detail: `${labels[pr.state]}${pr.reason ? ". " + pr.reason : ""}` };
      if (pr.state === "failed" || pr.state === "needs_user") it.warn = true;
      const run = (stp, method) => ({ device: T3_DEV, project: pr.id, step: stp, ...(method ? { method } : {}) });
      it.actions = [];
      if (can && pr.state !== "working") {
        if (pr.action === "login" || pr.action === "login_again") it.actions.push(act(pr.action === "login_again" ? "Sign in again" : "Sign in", "setup_run", run("login", "claudeai")));
        if (pr.action === "selftest") it.actions.push(act("Run self-test", "setup_run", run("selftest")));
      }
      items.push(it);
      for (const l of pr.progress || []) items.push({ title: l });
    }
    if (s.id === "login" && st.login) {
      const w = { title: "Sign-in waiting for the browser" + (st.login.role ? ` (role ${st.login.role})` : "") };
      let detail = "Waiting for Claude's sign-in link.";
      const wact = [];
      if (st.login.url) {
        detail = "Sign-in page (open it with the button): " + st.login.url + " After you sign in, Claude shows a code on its page: paste it here.";
        if (st.login.wants_code) detail = detail.replace("After you sign in, Claude shows", "Claude is waiting for the code: it shows");
        wact.push(act("Open the sign-in page", "setup_open", dev()));
        wact.push({ label: "Paste the code", op: "setup_code", args: dev(), prompt: { arg: "code", label: "Paste the code shown on the Claude page", value: "" } });
      }
      w.detail = detail;
      w.actions = wact;
      items.push(w);
    }
    sections.push({ title, empty: "", items });
  }
  sections.push({ title: "Project folders", empty: "No folder yet. Ask your Grok Bot to add the folder you want the agents to work in; you approve it with your passkey.",
    note: "Your Grok Bot adds a folder when you ask it to, and you approve each one. Rubi Home refuses system folders, your home folder itself and private folders in it.",
    items: get("projects").projects.map((p) => ({ title: p.dir, actions: busy ? [] : [{ label: "Remove", op: "setup_remove", args: { device: T3_DEV, project: p.id }, danger: true,
      confirm: "Remove this folder from the agent projects? Its files stay where they are." }] })) });
  sections.push(t3ModeSection());
  const page = { title: "Set up T3 Agents: " + T3_DEVNAME, up: T3_UP,
    note: "T3 runs on that computer and each role has its own VM. A role signs in to its provider on its own (Claude's `claude auth login`): this page only relays the sign-in link and the code you paste to Rubi Home on that computer. Installing the container tool shows macOS's own administrator dialog on the computer: this page never asks for a password, and it asks for your strong approval first. The folders the agents work in are added by your Grok Bot when you ask it, each with your strong approval.",
    sections };
  if (busy || st.login) page.refresh = 3;
  return page;
}
function t3ModeSection() {
  return { title: "Usage mode", empty: "", note: "How much of your Claude quota the agents may use. This sets one mode for every Bot and for new ones; you can change a single Bot later on the usage modes page. Only you change it: Bots cannot.",
    items: T3_MODES.map((m) => ({ title: m.label + (m.key === T3.usageDefault ? " (the current default)" : ""), detail: m.about,
      actions: [{ label: "Use for all Bots", op: "usage_mode", args: { device: T3_DEV, all: true, mode: m.key },
        confirm: `Set every Bot, and the default for new Bots, to ${m.label}? Running sessions are not stopped; the change applies to new ones.` }] })) };
}

// The plugin's first page: the entries, then what Rubi allows by itself (trusted sessions, trusted projects) and the bound Bots.
function t3MainPage() {
  const link = (title, detail, page) => ({ title, detail, actions: [{ label: "Open", open: t3Open(page) }] });
  const mitems = T3.mandates.map((m) => ({ title: `${T3_DEVNAME} — ${m.cwd}`,
    detail: `Session ${m.session} · ${m.uses} of ${m.max} commands · ${t3Min(m.expires - now())} min left`,
    actions: [{ label: "Revoke", op: "revoke", args: { id: m.id }, danger: true }] }));
  const all = { title: "All of them", actions: [{ label: "Revoke all", op: "revoke", args: { id: "all" }, danger: true, confirm: "Revoke every trust, 2-hour and project?" }] };
  if (mitems.length) mitems.push(all);
  const pitems = T3.trusts.map((p) => {
    const until = new Date(p.expires).toLocaleString("en-GB", { weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
    return p.unlimited
      ? { title: `UNTIL REVOKED — ${T3_DEVNAME} — ${p.root}`,
        detail: `UNTIL REVOKED — no end time, stays on across restarts and days · Branches ${p.branches} · ${p.uses} commands used (no limit) · granted ${t3Age(p.issued)}`,
        actions: [{ label: "Revoke", op: "revoke", args: { id: p.id }, danger: true }] }
      : { title: `${T3_DEVNAME} — ${p.root}`,
        detail: `Branches ${p.branches} · ${p.uses} of ${p.max} commands · until ${until} (${t3Min(p.expires - now())} min left) · granted ${t3Age(p.issued)}`,
        actions: [{ label: "Revoke", op: "revoke", args: { id: p.id }, danger: true }] };
  });
  if (pitems.length) pitems.push(all);
  const bitems = T3.bindings.map((b) => ({ title: `${b.sister} → ${b.title}`, detail: `${T3_DEVNAME} · ${b.repo} · session ${b.session} · since ${t3Stamp(b.since).replace("T", " ")}:00`,
    actions: [{ label: "Unbind", op: "unbind", args: { sister: b.sister }, danger: true, confirm: "Unbind " + b.sister + "?" }] }));
  return { title: "T3 Agents", refresh: 30,
    note: "Sessions running with bypassPermissions never ask, so approvals and trusts here don't apply to them (their messages to and from Bots still work).",
    sections: [
      { title: "Claude in project VMs", empty: "", note: "Claude run through the Agent SDK in a VM per project on your computer: the running sessions and queue, sessions with their chat, usage modes, usage and sign-in.", items: [
        link("Projects and sessions", "Each project's sessions with the running ones marked, what is waiting and why, and a button to start a new session.", "agent"),
        link("Usage modes", "How much each Bot may run at once: Maximum, Medium (Recommended) or Light. Only you change it.", "modes"),
        link("Agent roles", "Profiles of a project with their own skills, tools and hosts: the roles of each project, what each has, its history with a one-click revert, and a button to start a session in one.", "roles"),
        link("SSH targets", "Servers a role may run commands on, from your computer: add one (you confirm its host key), see which roles have it, remove it, and read what was run.", "ssh"),
        link("Usage and sign-in", "Today's estimate and whether Claude is signed in.", "usage")] },
      { title: "First-time setup", empty: "", items: [link("Set up T3 Agents", `On ${T3_DEVNAME}: Apple's container tool, the runner image, T3 (installed and pinned, then paired), each role's sign-in and a self-test, with buttons only.`, "setup")] },
      { title: "Trusted sessions (2 hours)", note: "Rubi allows these sessions' plain, listed commands by itself, including builds and tests that run the project's own code; push, --force, privileged commands, network commands, credential paths and writing outside the directory or into .git still ask.", empty: "None.", items: mitems },
      { title: "Trusted projects (until 04:00, or until revoked)", note: "A trust marked UNTIL REVOKED has no end time and no command limit: it stays on across restarts and days until you revoke it here (or with t3agents_mandates). Every Claude Code session on that computer inside the project: Rubi allows plain, listed commands by itself, including builds and tests that run the project's own code, and a Bot bound to a session of the project can start and bind sessions in it. Push and every remote git command, --force, privileged commands, network commands, tags, credential paths, writing outside the project and branches outside the pattern still ask.", empty: "None.", items: pitems },
      { title: "Bots bound to sessions", note: "Each Bot talks only to its session.", empty: "None.", items: bitems },
    ] };
}

function t3ProjectsPage() {
  const running = T3.sessions.filter((s) => s.running);
  const sections = [{ title: T3_DEVNAME, empty: "", note: `${t3Plural(running.length, "session", "sessions")} running of no cap on this computer, 1 at a time per project. A new session over a limit waits in the queue and says what it waits for.` }];
  for (const pr of T3.projects) {
    const mine = T3.sessions.filter((s) => s.project === pr.id).sort((a, b) => b.last - a.last);
    const items = mine.map((s) => ({ title: t3Label(s),
      detail: `${s.running ? "RUNNING now" + (s.subagents ? ` · ${t3Plural(s.subagents, "subagent", "subagents")}` : "") : s.busy ? "busy" : "idle"} · mode ${s.mode}${s.role ? " · role " + s.role : ""}`,
      actions: [{ label: "Open chat", open: t3Open("chat", { project: pr.id, session_id: s.id, ...(s.role ? { role: s.role } : {}) }) }] }));
    if (pr.id === "web-app") {
      items.push({ title: "A role session changed .mcp.json", detail: `Role reviewer · ${t3Stamp(now() - 2 * 3600e3)} · review before another session or your editor loads it`, warn: true });
      items.push({ title: "Waiting #1: a message to Review the cart PR in the role reviewer", detail: "waiting for a free place in this project · from Bot: Reviewer Bot", warn: true });
    }
    sections.push({ title: pr.dir, note: `VM: ${pr.vm} · ${mine.filter((s) => s.running).length} running of 1 at a time`, empty: "No sessions yet.", items,
      actions: [{ label: "New session", op: "new", args: { device: T3_DEV, project: pr.id }, prompt: { arg: "text", label: "First message for Claude in this project (sent as you)", value: "" } },
        { label: "Manage", open: t3Open("sessions", { project: pr.id }) }, { label: "Roles", open: t3Open("roles", { project: pr.id }) }] });
  }
  return { title: "Claude in project VMs", refresh: 15, up: T3_UP, sections };
}

function t3SessionsPage(a) {
  const pr = t3Need(t3Project(a.project), "project");
  const list = T3.sessions.filter((s) => s.project === pr.id).sort((x, y) => y.last - x.last);
  const rank = Object.fromEntries(T3_SESSION_MODES.map((m, i) => [m, i]));
  const itemsOf = {};
  for (const s of list) {
    const base = { device: T3_DEV, project: pr.id, session_id: s.id, ...(s.role ? { role: s.role } : {}) };
    const bound = T3.bindings.find((b) => b.session === s.id);
    const actions = [
      { label: "Open chat", open: t3Open("chat", { project: pr.id, session_id: s.id, ...(s.role ? { role: s.role } : {}) }) },
      { label: "Rename", op: "rename", args: base, prompt: { arg: "name", label: "Name for this session (up to 64 characters; leave empty to remove the name)", value: s.alias } },
      { label: "Fork", op: "fork", args: base, confirm: "Make a branch of this conversation as a new, idle session?" },
    ];
    if (s.busy) actions.push({ label: "Interrupt", op: "interrupt", args: base });
    for (const m of T3_SESSION_MODES.filter((x) => x !== s.mode)) actions.push({ label: "Mode: " + m + (rank[m] > rank[s.mode] ? " (asks you)" : ""), op: "mode", args: { ...base, mode: m } });
    actions.push({ label: "Close", op: "close", args: base, danger: true, confirm: "Close this session? Its history stays; it stops running." });
    (itemsOf[s.role] = itemsOf[s.role] || []).push({ title: t3Label(s), detail: `Sister: ${bound ? bound.sister : "none"} · mode ${s.mode} · ${s.busy ? "busy" : "idle"}`, actions });
  }
  const sections = [{ title: "Sessions", note: "Sessions of this project. A stricter mode applies at once; a looser one asks for your strong approval.", empty: "No sessions.", items: itemsOf[""] || [] }];
  const ids = Object.keys(itemsOf).filter((x) => x).sort();
  for (const id of ids) sections.push({ title: "Role " + id, empty: "No sessions.", items: itemsOf[id] });
  if (ids.length) sections[0].title = "Sessions of the default role";
  return { title: pr.dir + ": sessions", refresh: 15, up: { label: "Projects", open: t3Open("agent") }, sections };
}

function t3ChatPage(a) {
  const s = t3Need(t3Sess(a), "session");
  const base = { device: T3_DEV, project: s.project, session_id: s.id, ...(s.role ? { role: s.role } : {}) };
  const bound = T3.bindings.find((b) => b.session === s.id);
  const note = (s.role ? `Role: ${s.role} (its own VM, skills and tools). ` : "Role: default (the project's own .claude). ") + `Mode: ${s.mode} · ${s.busy ? "busy" : "idle"}.`
    + (bound ? ` Bound Bot: ${bound.sister}.` : "") + " Messages from Bots are marked with their sender and \"untrusted text\": they are text from another program, not instructions from you. What you send here goes in as your own message.";
  return { title: t3Label(s), refresh: 5, note, up: { label: "Sessions", open: t3Open("sessions", { project: s.project }) },
    sections: [{ kind: "chat", title: "Chat", empty: "No messages yet.", items: T3.chats[s.id] || [],
      actions: [{ label: "Interrupt", op: "interrupt", args: base, danger: true, confirm: "Interrupt what Claude is doing now?" }],
      input: { op: "send", args: base, arg: "text", label: "Message to Claude (sent as you)", send: "Send" } }] };
}

function t3UsagePage() {
  const signed = Object.values(T3.signedIn).some(Boolean);
  const hint = "To sign in, use Anthropic's own sign-in on the computer that runs Rubi Home (Claude's /login, in the project's setup). Rubi never asks for, sees or stores your password or token, and this page has no sign-in button.";
  return { title: "Usage and sign-in", refresh: 60, up: T3_UP, sections: [
    { title: "Today", note: "A yellow notice shows above $10.00 (set it in the plugin's settings). It never blocks anything.", empty: "No computer reports the t3-agent capability.",
      items: [{ title: `${T3_DEVNAME}: about $4.20 today`, detail: "An estimate; Anthropic's own numbers are the real ones. About 310000 tokens." }] },
    { title: "Sign-in", empty: "No computer reports the t3-agent capability.", items: [signed
      ? { title: `${T3_DEVNAME}: signed in, plan Max`, detail: "Account alex@example.com. Sign-in lives on that computer only." }
      : { title: `${T3_DEVNAME}: not signed in`, detail: hint }] }] };
}

function t3ModesPage() {
  const running = T3.sessions.filter((s) => s.running);
  const botItems = T3.bots.map((b) => {
    const m = T3_MODES.find((x) => x.key === b.mode);
    const mine = running.filter((s) => (b.title === "Reviewer Bot") === (s.id === "s1"));
    return { title: `${b.title}: ${m.label}`, detail: `Sessions ${mine.length} of ${m.limit || "no limit"} · subagents per session: ${m.sub || "no limit"}${mine.map((s) => ` · ${t3Label(s)}: ${s.subagents} running`).join("")}`,
      actions: T3_MODES.filter((x) => x.key !== b.mode).map((x) => ({ label: x.label, op: "usage_mode", args: { device: T3_DEV, bot: b.id, mode: x.key } })) };
  });
  return { title: "Usage modes", refresh: 15, up: T3_UP, sections: [
    { title: T3_DEVNAME + ": modes", note: T3_MODES.map((m) => `${m.label}: ${m.about}`).join(" ") + " The limits are per Bot: a Bot on Light never limits another on Medium. A change applies to new sessions; nothing running is stopped, and when lowered the extra sessions finish and new work waits. Only you change this: Bots cannot.",
      empty: "", items: [
        { title: "Default for new Bots and your own sessions: " + T3_MODES.find((m) => m.key === T3.usageDefault).label,
          detail: `Sessions running on this computer: ${running.length} of no limit (a cap set in Rubi Home's configuration), one per project at a time unless configured.`,
          actions: T3_MODES.filter((m) => m.key !== T3.usageDefault).map((m) => ({ label: "Default: " + m.label, op: "usage_mode", args: { device: T3_DEV, default: true, mode: m.key } })) },
        { title: "Set one mode for all Bots", detail: "Applies to every Bot Rubi Home knows and to the default. Afterwards you can change Bots one by one again.",
          actions: T3_MODES.map((m) => ({ label: "All Bots: " + m.label, op: "usage_mode", args: { device: T3_DEV, all: true, mode: m.key },
            confirm: `Set every Bot, and the default for new Bots, to ${m.label}? Running sessions are not stopped; the change applies to new ones.` })) }] },
    { title: T3_DEVNAME + ": Bots", empty: "No Bot has used the agent path yet; a new Bot gets the default mode.", items: botItems }] };
}

function t3RolesPage(a) {
  const sections = [];
  for (const pr of T3.projects) {
    if (a.project && pr.id !== a.project) continue;
    const runIn = (role) => T3.sessions.filter((s) => s.project === pr.id && s.running && s.role === role).length;
    const items = [{ title: "default (the project's own .claude)",
      detail: `Shared with everyone working in the repository; Rubi can't version or isolate it. ${t3Plural(runIn(""), "session", "sessions")} running. Convert makes a new isolated role on this project; the sessions it already has stay with this role.`,
      actions: [{ label: "Start session", op: "new", args: { device: T3_DEV, project: pr.id }, prompt: { arg: "text", label: "First message for Claude in the default role (sent as you)", value: "" } },
        { label: "Convert to a new role", op: "role_convert", args: { device: T3_DEV, project: pr.id }, prompt: { arg: "text", label: "Name for the new role (a-z, 0-9 and -, up to 32 characters)", value: "" } }] }];
    const mine = T3.roles.filter((r) => r.project === pr.id).sort((x, y) => (x.status === "active" ? 0 : 1) - (y.status === "active" ? 0 : 1) || x.name.localeCompare(y.name));
    for (const r of mine) {
      const held = r.skills.filter((k) => k.state === "held").length, un = r.skills.filter((k) => k.source === "adhoc" && k.state !== "held" && !k.accepted).length;
      const outside = r.tools.filter((t) => !T3_SAFE_TOOLS.includes(t));
      let detail = `${r.status} · version ${r.version} · ${t3Plural(r.skills.length, "skill", "skills")} · ${t3Plural(r.tools.length, "tool", "tools")}${outside.length ? ` (outside the safe set: ${outside.join(", ")})` : ""} · mode ${r.mode}`;
      if (held) detail += ` · ${held} HELD (need your accept)`;
      if (un) detail += ` · ${un} unreviewed`;
      if (runIn(r.id)) detail += ` · ${t3Plural(runIn(r.id), "session", "sessions")} running`;
      if (r.description) detail += " · " + r.description;
      const args = { device: T3_DEV, role: r.id };
      const actions = [{ label: "Open", open: t3Open("role", { role: r.id, project: r.project }) }];
      if (r.status === "active") {
        actions.push({ label: "Start session", op: "new", args: { device: T3_DEV, project: r.project, role: r.id }, prompt: { arg: "text", label: "First message for Claude in this role (sent as you)", value: "" } });
        actions.push({ label: "Archive", op: "role_archive", args, danger: true, confirm: "Archive this role? Nothing is deleted; it can't start sessions until you unarchive it." });
      } else actions.push({ label: "Unarchive", op: "role_unarchive", args });
      items.push({ title: r.name + (r.status !== "active" ? " (archived)" : ""), detail, actions, ...(held ? { warn: true } : {}) });
    }
    sections.push({ title: pr.dir, note: "A role is a profile of one project: its own skills, tools, hosts and instructions, in a VM of its own, so what one role is given never reaches another. The project's default role is the repository's own .claude and can't be changed here.", empty: "", items,
      actions: [{ label: "New role", op: "role_new", args: { device: T3_DEV, project: pr.id }, prompt: { arg: "text", label: "Name for the new role (a-z, 0-9 and -, up to 32 characters)", value: "" } }] });
  }
  return { title: "Agent roles", refresh: 15, up: { label: "Projects", open: t3Open("agent") }, sections };
}

function t3RolePage(a) {
  const r = t3Need(t3Role(a.role), "role");
  const args = (extra = {}) => ({ device: T3_DEV, role: r.id, ...extra });
  const remove = (op, item, ...more) => [{ label: "Remove", op, args: args({ item }), danger: true }, ...more];
  const held = r.skills.filter((k) => k.state === "held").length;
  const acts = [{ label: "History", open: t3Open("role_history", { role: r.id, project: r.project }) }];
  if (r.status === "active") {
    acts.push({ label: "Start session", op: "new", args: { device: T3_DEV, project: r.project, role: r.id }, prompt: { arg: "text", label: "First message for Claude in this role (sent as you)", value: "" } });
    acts.push({ label: "Archive", op: "role_archive", args: args(), danger: true, confirm: "Archive this role? Nothing is deleted; it can't start sessions until you unarchive it." });
  } else acts.push({ label: "Unarchive", op: "role_unarchive", args: args() });
  const sections = [{ title: "Role", empty: "", items: [
    { title: r.name, detail: `${r.status} · version ${r.version} · created by ${r.createdBy} · last changed by ${r.changedBy}${r.description ? " · " + r.description : ""}`, actions: acts },
    { title: "Instructions", detail: "412 characters of text written by Bots or by you elsewhere. The panel's boxes hold 200 characters, so instructions and skill text are not edited or shown here; read and revert them through History." }] }];
  const skills = r.skills.map((k) => {
    let tag = k.source === "library" ? (k.pin ? `library ${k.libId}@${k.pin} (pinned)` : `library ${k.libId} (following the latest admitted, now version ${k.resolved})`) : `written by ${k.addedBy}`;
    if (k.state === "held") tag += " · HELD: the scan flagged it, so it isn't used until you accept it";
    else if (k.source === "adhoc" && !k.accepted) tag += " · unreviewed";
    if (k.verdict) tag += " · scan: " + k.verdict;
    if (k.rules) tag += " · rules " + k.rules;
    const it = { title: k.name, detail: tag };
    if (k.state === "held") { it.warn = true; it.actions = remove("role_skill_remove", k.name, { label: "Accept", op: "role_skill_accept", args: args({ item: k.name }) }); }
    else if (k.source === "library") it.actions = remove("role_skill_remove", k.name, k.pin ? { label: "Follow the latest", op: "role_skill_follow", args: args({ item: k.name }) } : { label: "Pin to this version", op: "role_skill_pin", args: args({ item: k.name }) });
    else it.actions = remove("role_skill_remove", k.name);
    return it;
  });
  sections.push({ title: "Skills", note: "A skill is text Claude reads; it grants no tools, hosts or permissions. Bots add skills; you can remove them, and accept a held one. A library skill is shared and signed by Rubi: one that follows the latest is updated here when a newer version is admitted (the History shows each update and can revert it) and dropped if the curator withdraws it; pinning keeps its version." + (held ? ` ${held} held, 0 unreviewed.` : ""), empty: "No skills.", items: skills });
  sections.push({ title: "Tools", note: "The safe set turns on at once. A tool outside it asks for your strong approval first, and so does anything else that widens the role.", empty: "", items: T3_TOOLS.map((t) => {
    const on = r.tools.includes(t), safe = T3_SAFE_TOOLS.includes(t);
    return { title: t, detail: (on ? "allowed" : "not allowed") + (safe ? "" : " · outside the safe set"),
      actions: [on ? { label: "Turn off", op: "role_tool", args: args({ item: t, on: false }) } : { label: safe ? "Turn on" : "Turn on (asks you)", op: "role_tool", args: args({ item: t, on: true }) }] };
  }) });
  sections.push({ title: "Deny rules", note: "Claude is never allowed what a deny rule names. Removing one you set is immediate; removing one a Bot or the library set asks for your strong approval.", empty: "None.",
    items: r.deny.map((x) => ({ title: x.rule, detail: "set by " + x.by, actions: [{ label: x.by === "you" ? "Remove" : "Remove (asks you)", op: "role_deny_remove", args: args({ item: x.rule }), danger: true }] })),
    actions: [{ label: "Add a deny rule", op: "role_deny_add", args: args(), prompt: { arg: "text", label: "Deny rule, such as WebFetch or Bash(git push:*) (up to 200 characters)", value: "" } }] });
  sections.push({ title: "MCP servers", note: "MCP servers run code. A server's address and command don't fit the panel's 200-character box, so the panel only removes them; Bots add them (each needs your strong approval).", empty: "None.",
    items: r.mcp.map((m) => ({ title: m.name, detail: `${m.type} · ${m.url} · approval ${m.approval}`, actions: remove("role_mcp_remove", m.name) })) });
  sections.push({ title: "Claude plugins", note: "Adding a Claude plugin is done by Bots and asks for your strong approval; the panel only removes them.", empty: "None.",
    items: r.plugins.map((m) => ({ title: m.name, detail: `from ${m.source} · approval ${m.approval}`, actions: remove("role_plugin_remove", m.name) })) });
  sections.push({ title: "Network hosts", note: "Beyond the base list. Adding a host asks for your strong approval when a Bot asks; the panel only removes them.", empty: "None.",
    items: r.hosts.map((x) => ({ title: x, detail: "the role's VM may reach this host", actions: remove("role_host_remove", x) })) });
  sections.push({ title: "SSH grants", note: "A target is registered on the SSH targets page, by you only. Granting a registered target is immediate; the role can then run the target's allowed commands from its VM.", empty: "No SSH targets are registered.",
    items: T3.ssh.map((t) => {
      const granted = r.ssh.includes(t.name);
      const detail = `${t.user}@${t.host}:${t.port} · ${granted ? "this role may use it" : "not granted"}`;
      return { title: t.name, detail: t.changed ? "HOST KEY CHANGED, connections are refused · " + detail : detail, ...(t.changed ? { warn: true } : {}),
        actions: [granted ? { label: "Revoke", op: "role_ssh_revoke", args: args({ item: t.name }), danger: true } : { label: "Grant", op: "role_ssh_grant", args: args({ item: t.name }) }] };
    }),
    actions: [{ label: "SSH targets", open: t3Open("ssh") }] });
  sections.push({ title: "Subagents", note: "A subagent can't have more tools than the role.", empty: "None.",
    items: r.agents.map((x) => ({ title: x.name, detail: `model ${x.model} · tools ${x.tools.join(", ")} · ${x.description}`, actions: remove("role_agent_remove", x.name) })) });
  const rank = Object.fromEntries(T3_SESSION_MODES.map((m, i) => [m, i]));
  sections.push({ title: "Mode and project instructions", empty: "", items: [
    { title: "Default permission mode: " + r.mode, detail: `The mode a session of this role starts in. Up to the ceiling set in the plugin's settings (${T3_CEILING}) it changes at once; above it asks for your strong approval.`,
      actions: T3_SESSION_MODES.filter((m) => m !== r.mode).map((m) => ({ label: m + (rank[m] > rank[T3_CEILING] ? " (asks you)" : ""), op: "role_mode", args: args({ mode: m }) })) },
    { title: "Project instructions: " + (r.pinstr ? "on" : "off"), detail: "Whether Claude reads the repository's own CLAUDE.md in this role.",
      actions: [{ label: r.pinstr ? "Turn off" : "Turn on", op: "role_pinstr", args: args({ on: !r.pinstr }) }] }] });
  return { title: "Role " + r.name, refresh: 30, up: { label: "Roles", open: t3Open("roles", { project: r.project }) }, sections };
}

function t3HistoryPage(a) {
  const r = t3Need(t3Role(a.role), "role");
  const items = r.history.map((v) => {
    const cur = v.version === r.version;
    const acts = [{ label: "Show changes", open: t3Open("role_diff", { role: r.id, version: v.version }) }];
    if (!cur) acts.push({ label: "Revert to this version", op: "role_revert", args: { device: T3_DEV, role: r.id, version: v.version }, danger: true,
      confirm: "Make a new version with this version's content? Anything it gives the role that the current version lacks asks for your strong approval." });
    return { title: `Version ${v.version}${cur ? " (current)" : ""}`, detail: `by ${v.by} · ${t3Stamp(v.at)} · tier ${v.tier} (${v.tier === 2 ? "you approved it or made it" : "no approval needed"}) · ${v.op} · ${v.summary}`, actions: acts };
  });
  return { title: "History of the role", refresh: 30, up: { label: "Role", open: t3Open("role", { role: r.id }) },
    sections: [{ title: "Versions, newest first", empty: "No versions.", items,
      note: "A revert is a new version; nothing is erased from the history. A revert that gives the role something it doesn't have now (an MCP server, a host, a tool outside the safe set, a looser mode) asks for your strong approval; any other revert is applied at once." }] };
}
function t3DiffPage(a) {
  const r = t3Need(t3Role(a.role), "role");
  const v = t3Need(r.history.find((x) => x.version === a.version), "version");
  const up = { label: "History", open: t3Open("role_history", { role: r.id }) };
  const head = `Version ${v.version}, by ${v.by}`;
  const acts = v.version !== r.version ? [{ label: "Revert to this version", op: "role_revert", args: { device: T3_DEV, role: r.id, version: v.version }, danger: true,
    confirm: "Make a new version with this version's content? Anything it gives the role that the current version lacks asks for your strong approval." }] : [];
  if (v.version === 1) return { title: "Version 1", up, sections: [{ title: head, note: "The first version of the role: there is nothing before it to compare with.", empty: "", items: [], actions: acts }] };
  const show = (x) => (x === undefined ? "" : JSON.stringify(x));
  const changes = v.changes.map((c) => ({ title: `${c.field}${c.key ? " " + c.key : ""}: ${c.kind}`, detail: c.from !== undefined && c.to !== undefined ? `${show(c.from)} -> ${show(c.to)}` : show(c.to !== undefined ? c.to : c.from) }));
  return { title: `Version ${v.version} of the role`, up, sections: [{ title: `${head}, compared with version ${v.version - 1}`, empty: "No structural changes.", items: changes, actions: acts,
    note: "Text of instructions and descriptions is shown clipped. This is what the version changed; Revert makes a new version equal to it." }] };
}

function t3SshPage() {
  const rowOf = (t) => {
    const allowed = t.allowed.length ? "only " + t.allowed.join("; ") : "any command";
    const roles = T3.roles.filter((r) => r.ssh.includes(t.name)).map((r) => r.name);
    let detail = `${t.user}@${t.host}:${t.port} · host key ${t3Fp(t.name)} (${t.key}) · ${allowed} · ${t.ask ? "asks you for each command" : "runs without asking"} · ${roles.length ? "granted to " + roles.join(", ") : "no role has it"}`;
    const it = { title: t.name, actions: [{ label: "Audit", open: t3Open("ssh_audit", { target: t.name }) },
      { label: "Remove", op: "ssh_remove", args: { device: T3_DEV, item: t.name }, danger: true,
        confirm: "Remove this target? Its key is deleted, streams open to it are cut and roles lose the grant. You would have to remove the key from the server yourself." }] };
    if (t.allowed.some((c) => c.trim().endsWith(" **"))) { it.warn = true; detail = "WARNING: ls ** lets the role pass any options, including ones that run other programs · " + detail; }
    if (t.changed) { it.warn = true; detail = "HOST KEY CHANGED: the server presented another host key than the one pinned, so Rubi Home refuses to connect. This can be a reinstalled server or someone in between; check, then remove and add the target again. · " + detail; }
    it.detail = detail;
    return it;
  };
  const sections = [{ title: "SSH targets", note: "A target is a server a role may run commands on, from your computer: Rubi Home connects, so the project VM never sees a key or a host. Only you register a target (a Bot can't). A role gets a target only when it is granted: you grant it on the role's page, and a Bot with a project trust may grant a target with restricted commands or ask-each-time to its roles. A target that allows any command needs your approval for each grant. The target's allowed commands apply. Names, hosts and commands are shown as text.",
    empty: "No targets registered.", items: T3.ssh.map(rowOf),
    actions: [{ label: "Add a target", op: "ssh_add", args: { device: T3_DEV }, prompt: { arg: "text", label: "Name, host, user and optionally port, separated by spaces (for example: prod db.example.com deploy 22)", value: "" } }] }];
  const keys = T3.ssh.filter((t) => T3.keys[t.name]).map((t) => ({ title: "authorized_keys line for " + t.name, detail: T3.keys[t.name], warn: true }));
  if (keys.length) sections.push({ title: "Install the public key", note: "Add this line to ~/.ssh/authorized_keys of the user on the server. It is a public key (safe to copy); the private key stays in Rubi Home on your computer. It is shown until Rubi's plugin restarts.", empty: "", items: keys });
  if (T3.drafts.length) {
    const di = T3.drafts.map((d) => {
      const args = { device: T3_DEV, item: d.name };
      const allowed = d.allowed.length ? "only " + d.allowed.join("; ") : "any command";
      return { title: d.name, detail: `${d.user}@${d.host}:${d.port} · ${allowed} · ${d.ask ? "asks you for each command" : "runs without asking"}${d.reuse ? " · reuses the name of a removed target (roles that still list it get this one)" : ""}`, warn: false, actions: [
        { label: "Check the host key", op: "ssh_confirm", args, prompt: { arg: "text", label: "Host key fingerprint you expect (SHA256:...). Leave empty to compare it in the approval instead", value: "" } },
        { label: "Allow a command", op: "ssh_draft_cmd", args, prompt: { arg: "text", label: "A command, or a prefix ending in * (more arguments, no options) or ** (any arguments, options too: it lets the role pass options that run other programs), such as tail -n * (up to 200 characters)", value: "" } },
        { label: "Allow any command", op: "ssh_draft_anycmd", args },
        { label: d.ask ? "Don't ask for each command" : "Ask for each command", op: "ssh_draft_ask", args: { ...args, on: !d.ask } },
        { label: d.reuse ? "Don't reuse a removed name" : "Reuse a removed name", op: "ssh_draft_reuse", args: { ...args, on: !d.reuse } },
        { label: "Discard", op: "ssh_draft_discard", args, danger: true }] };
    });
    const sec = { title: "Targets being added", note: "Nothing is registered until you confirm the host key in a strong approval. Choose the allowed commands and whether each command asks you, then press Check the host key.", empty: "", items: di };
    if (T3.drafts.length > 1) sec.actions = [{ label: "Check all host keys", op: "ssh_confirm_all", args: { device: T3_DEV } }];
    sections.push(sec);
  }
  return { title: "SSH targets", refresh: 20, up: { label: "Projects", open: t3Open("agent") }, sections };
}
function t3AuditPage(a) {
  const t = t3Need(T3.ssh.find((x) => x.name === a.target), "target");
  const items = (T3.audit[t.name] || []).map((e) => ({ title: `${t3Stamp(e.at)} · ${e.result}`,
    detail: `role ${e.role} · by ${e.bot}${e.exit !== undefined ? ` · exit ${e.exit}` : ""} · ${e.bin} bytes in, ${e.bout} out · command: ${e.command}${e.reason ? " · " + e.reason : ""}`,
    ...(e.result !== "ok" ? { warn: true } : {}) }));
  return { title: "Audit of " + t.name, refresh: 20, up: { label: "SSH targets", open: t3Open("ssh") },
    sections: [{ title: "Newest first", note: "Every stream a role opened to this target, and every refusal (an ungranted role, a command that isn't allowed, a changed host key). Commands are the Bots' own text, shown clipped; no output and no key is ever recorded.", empty: "Nothing yet.", items }] };
}

function t3View(a) {
  switch (a.page || "") {
    case "": return t3MainPage();
    case "agent": return t3ProjectsPage();
    case "sessions": return t3SessionsPage(a);
    case "chat": return t3ChatPage(a);
    case "usage": return t3UsagePage();
    case "setup": return t3SetupPage();
    case "modes": return t3ModesPage();
    case "roles": return t3RolesPage(a);
    case "role": return t3RolePage(a);
    case "role_history": return t3HistoryPage(a);
    case "role_diff": return t3DiffPage(a);
    case "ssh": return t3SshPage();
    case "ssh_audit": return t3AuditPage(a);
  }
  throw new Error(`unknown page "${a.page}"`);
}

// A role changes: a new version, with what changed.
function t3Change(r, summary, op, changes) {
  r.version++;
  r.changedBy = "you";
  r.history.unshift({ version: r.version, by: "you", at: now(), tier: 2, op, summary, changes });
}
const T3_ROLE_NAME = /^[a-z0-9-]{1,32}$/;
function t3NewRole(a, convert) {
  const name = (a.text || "").trim();
  if (!T3_ROLE_NAME.test(name)) throw new Error("A role name is a-z, 0-9 and -, up to 32 characters.");
  if (T3.roles.some((r) => r.project === a.project && r.name === name)) throw new Error("this project already has a role with that name");
  T3.roles.push({ id: name, project: a.project, name, status: "active", version: 1, createdBy: "you", changedBy: "you", description: convert ? "Converted from the project's default role" : "",
    mode: "default", pinstr: true, tools: ["Read", "Edit", "Write", "Glob", "Grep", "Bash"], skills: [], deny: [], mcp: [], plugins: [], hosts: [], ssh: [], agents: [],
    history: [{ version: 1, by: "you", at: now(), tier: 2, op: "role.create", summary: "Created the role", changes: [] }] });
}
function t3Session(a, text, extra = {}) {
  const id = "s" + T3.nextSession++;
  const s = { id, project: a.project, role: a.role || "", runner: id, alias: text.split("\n")[0].slice(0, 40), mode: "default", busy: true, running: true, subagents: 0, last: now(), ...extra };
  T3.sessions.unshift(s);
  T3.chats[id] = [{ role: "user", author: "you", text }, { role: "assistant", author: "Claude", text: "Reading the project first. I'll tell you what I find." }];
  return s;
}
function t3Do(op, a) {
  const ok = { ok: true };
  switch (op) {
    case "setup_install": T3.setup.container = "done"; return ok;
    case "setup_run": {
      switch (a.step) {
        case "service": t3Start("service", "", 2); break;
        case "image": t3Start("image", "", 5, T3_BUILD); break;
        case "t3": t3Start("t3", "", 3); break;
        case "pair": t3Start("pair", "", 2); break;
        case "all": t3Start("all", "", 7, T3_BUILD); break;
        case "login":
          if (a.method) T3.flow = { role: a.project || "reviewer", at: now(), wantsCode: false };
          else t3Start("check", a.project, 1.2);
          break;
        case "selftest": {
          t3Need(t3Project(a.project), "project");
          if (!Object.values(T3.signedIn).some(Boolean)) {
            T3.selftest[a.project] = { state: "failed", reason: "Claude is not signed in on this computer: sign in a role first", lines: [] };
            break;
          }
          t3Start("selftest", a.project, 4, T3_SELFTEST);
          break;
        }
        case "signout": for (const k of Object.keys(T3.signedIn)) T3.signedIn[k] = false; T3.selftest = {}; break;
        default: throw new Error("unknown step");
      }
      return ok;
    }
    case "setup_open":
      if (!T3.flow) throw new Error("No sign-in is waiting.");
      T3.flow.wantsCode = true;
      return ok;
    case "setup_code": {
      const code = (a.code || "").trim();
      if (!T3.flow) throw new Error("No sign-in is waiting.");
      if (!code) throw new Error("no code");
      if (!/^[!-~]{1,512}$/.test(code)) throw new Error("the code is plain characters without spaces");
      t3Start("login", T3.flow.role, 2.5);
      T3.flow = null;
      return ok;
    }
    case "setup_cancel": T3.job = null; T3.flow = null; return ok;
    case "setup_remove": {
      const i = T3.projects.findIndex((p) => p.id === a.project);
      if (i < 0) throw new Error("no such project");
      T3.projects.splice(i, 1);
      return ok;
    }
    case "usage_mode": {
      const m = T3_MODES.find((x) => x.key === a.mode);
      if (!m) throw new Error("no such mode");
      if (a.all) { T3.usageDefault = m.key; T3.bots.forEach((b) => { b.mode = m.key; }); }
      else if (a.default) T3.usageDefault = m.key;
      else t3Need(T3.bots.find((b) => b.id === a.bot), "Bot").mode = m.key;
      return ok;
    }
    case "interrupt": { const s = t3Need(t3Sess(a), "session"); s.busy = s.running = false; return ok; }
    case "close": T3.sessions.splice(T3.sessions.indexOf(t3Need(t3Sess(a), "session")), 1); return ok;
    case "fork": { const s = t3Need(t3Sess(a), "session"); const f = t3Session({ project: s.project, role: s.role }, "(a branch of " + t3Label(s) + ")", { mode: s.mode, busy: false, running: false }); f.alias = t3Label(s) + " (fork)"; T3.chats[f.id] = [...T3.chats[s.id]]; return ok; }
    case "new": {
      const text = (a.text || "").trim();
      if (!text) throw new Error("nothing to send");
      t3Need(t3Project(a.project), "project");
      t3Session(a, text);
      return ok;
    }
    case "send": {
      const s = t3Need(t3Sess(a), "session"), text = (a.text || "").trim();
      if (!text) throw new Error("nothing to send");
      T3.chats[s.id].push({ role: "user", author: "you", text }, { role: "assistant", author: "Claude", text: "Got it. I'll start with the failing test and report back." });
      s.last = now();
      return { queued: true, position: 1 };
    }
    case "rename": {
      const s = t3Need(t3Sess(a), "session"), name = (a.name || "").trim();
      if (name.length > 64) throw new Error("At most 64 characters.");
      s.alias = name;
      return ok;
    }
    case "mode": {
      if (!T3_SESSION_MODES.includes(a.mode)) throw new Error("no such mode");
      t3Need(t3Sess(a), "session").mode = a.mode;
      return ok;
    }
    case "revoke":
      for (const k of ["mandates", "trusts"]) T3[k] = a.id === "all" ? [] : T3[k].filter((x) => x.id !== a.id);
      return { revoked: 1 };
    case "unbind": T3.bindings = T3.bindings.filter((b) => b.sister !== a.sister); return { unbound: true };
    case "role_new": t3NewRole(a, false); return ok;
    case "role_convert": t3NewRole(a, true); return ok;
    case "role_archive": case "role_unarchive": {
      const r = t3Need(t3Role(a.role), "role");
      r.status = op === "role_archive" ? "archived" : "active";
      t3Change(r, op === "role_archive" ? "Archived" : "Unarchived", op.replace("_", "."), [{ field: "status", kind: "changed", from: op === "role_archive" ? "active" : "archived", to: r.status }]);
      return ok;
    }
    case "role_skill_remove": case "role_skill_accept": case "role_skill_pin": case "role_skill_follow": {
      const r = t3Need(t3Role(a.role), "role"), k = t3Need(r.skills.find((x) => x.name === a.item), "skill");
      if (op === "role_skill_remove") r.skills = r.skills.filter((x) => x !== k);
      else if (op === "role_skill_accept") { k.state = "ok"; k.accepted = true; k.verdict = k.rules = ""; }
      else k.pin = op === "role_skill_pin" ? k.resolved : "";
      t3Change(r, `${op.slice(11)} skill ${k.name}`, op.replace("_", "."), [{ field: "skills", key: k.name, kind: op === "role_skill_remove" ? "removed" : "changed" }]);
      return ok;
    }
    case "role_tool": {
      const r = t3Need(t3Role(a.role), "role");
      if (!T3_TOOLS.includes(a.item)) throw new Error("no such tool");
      r.tools = a.on ? [...r.tools, a.item] : r.tools.filter((x) => x !== a.item);
      t3Change(r, `${a.on ? "Turned on" : "Turned off"} ${a.item}`, "tool.set", [{ field: "tools", key: a.item, kind: a.on ? "added" : "removed", to: !!a.on }]);
      return ok;
    }
    case "role_deny_add": {
      const r = t3Need(t3Role(a.role), "role"), rule = (a.text || "").trim();
      if (!rule || rule.length > 200) throw new Error("A deny rule is 1 to 200 characters.");
      r.deny.push({ rule, by: "you" });
      t3Change(r, "Added a deny rule", "deny.add", [{ field: "deny", key: rule, kind: "added" }]);
      return ok;
    }
    case "role_deny_remove": case "role_mcp_remove": case "role_plugin_remove": case "role_host_remove": case "role_agent_remove": case "role_ssh_revoke": {
      const r = t3Need(t3Role(a.role), "role");
      const field = { role_deny_remove: "deny", role_mcp_remove: "mcp", role_plugin_remove: "plugins", role_host_remove: "hosts", role_agent_remove: "agents", role_ssh_revoke: "ssh" }[op];
      r[field] = r[field].filter((x) => (x.rule || x.name || x) !== a.item);
      t3Change(r, `Removed ${a.item}`, op.replace("_", "."), [{ field, key: a.item, kind: "removed" }]);
      return ok;
    }
    case "role_ssh_grant": {
      const r = t3Need(t3Role(a.role), "role");
      t3Need(T3.ssh.find((t) => t.name === a.item), "target");
      if (!r.ssh.includes(a.item)) r.ssh.push(a.item);
      t3Change(r, `Granted ${a.item}`, "ssh.grant", [{ field: "ssh", key: a.item, kind: "added" }]);
      return ok;
    }
    case "role_mode": {
      const r = t3Need(t3Role(a.role), "role");
      if (!T3_SESSION_MODES.includes(a.mode)) throw new Error("no such mode");
      t3Change(r, `Mode ${a.mode}`, "mode.set", [{ field: "default_mode", kind: "changed", from: r.mode, to: a.mode }]);
      r.mode = a.mode;
      return ok;
    }
    case "role_pinstr": {
      const r = t3Need(t3Role(a.role), "role");
      r.pinstr = !!a.on;
      t3Change(r, `Project instructions ${a.on ? "on" : "off"}`, "pinstr.set", [{ field: "project_instructions", kind: "changed", to: !!a.on }]);
      return ok;
    }
    case "role_revert": {
      const r = t3Need(t3Role(a.role), "role");
      t3Need(r.history.find((x) => x.version === a.version), "version");
      t3Change(r, `Reverted to version ${a.version}`, "role.revert", [{ field: "role", kind: "reverted", to: a.version }]);
      return ok;
    }
    case "ssh_add": {
      const [name, host, user, port] = (a.text || "").trim().split(/\s+/);
      if (!/^[a-z0-9][a-z0-9-]{0,31}$/.test(name || "") || !host || !user) throw new Error("Write the name, the host and the user, and optionally the port, separated by spaces.");
      const p = port ? Number(port) : 22;
      if (!(p >= 1 && p <= 65535)) throw new Error("The port is a number from 1 to 65535.");
      if (T3.ssh.some((t) => t.name === name) || T3.drafts.some((d) => d.name === name)) throw new Error("a target with that name exists");
      T3.drafts.push({ name, host, user, port: p, allowed: [], ask: false, reuse: false });
      return ok;
    }
    case "ssh_confirm": case "ssh_confirm_all": {
      const take = op === "ssh_confirm" ? [t3Need(T3.drafts.find((d) => d.name === a.item), "target")] : [...T3.drafts];
      if (op === "ssh_confirm" && a.text && a.text.trim() !== t3Fp(take[0].name)) throw new Error("The host key is not the one you expected: nothing was registered.");
      for (const d of take) {
        T3.ssh.push({ name: d.name, host: d.host, user: d.user, port: d.port, key: "ssh-ed25519", allowed: d.allowed, ask: d.ask, changed: false });
        T3.keys[d.name] = "ssh-ed25519 AAAAdemoPublicKeyForTheDemoOnly" + t3Fp(d.name).slice(7, 27) + " t3agents-" + d.name;
        T3.drafts = T3.drafts.filter((x) => x !== d);
      }
      return ok;
    }
    case "ssh_draft_cmd": case "ssh_draft_anycmd": case "ssh_draft_ask": case "ssh_draft_reuse": case "ssh_draft_discard": {
      const d = t3Need(T3.drafts.find((x) => x.name === a.item), "target");
      if (op === "ssh_draft_cmd") {
        const c = (a.text || "").trim();
        if (!c || c.length > 200 || /\n/.test(c)) throw new Error("A command is one line, up to 200 characters.");
        d.allowed.push(c);
      } else if (op === "ssh_draft_anycmd") d.allowed = [];
      else if (op === "ssh_draft_ask") d.ask = !!a.on;
      else if (op === "ssh_draft_reuse") d.reuse = !!a.on;
      else T3.drafts = T3.drafts.filter((x) => x !== d);
      return ok;
    }
    case "ssh_remove":
      T3.ssh = T3.ssh.filter((t) => t.name !== a.item);
      T3.roles.forEach((r) => { r.ssh = r.ssh.filter((x) => x !== a.item); });
      return ok;
  }
  throw new Error(`unknown op "${op}"`);
}
export function demoT3Page(op, a = {}) {
  t3Tick();
  return op === "view" ? t3View(a) : t3Do(op, a);
}

// The skill library: a few generic skills, one self-admitted by the curator, one proposal with a flag.
const DEMO_LIB = {
  curator: "Librarian",
  versions: {
    "design-tokens": [
      { version: 1, state: "deprecated", description: "Use the shared design tokens for colors and spacing.", tags: ["design", "css"], author_bot: "Designer", submitted_at: iso(now() - 86400e3 * 20),
        admitted_by: "Librarian", admitted_at: iso(now() - 86400e3 * 19), scan_verdict: "pass", scanner_version: "1", sha256: "9f2c5a1e7b3d4c80a6e1f0b92d7c3a54e8b6f1d0c2a79e43b5d8f6a1c0e7b392",
        content: "---\nname: design-tokens\ndescription: Use the shared design tokens for colors and spacing.\n---\n# Design tokens\nUse the tokens, never raw hex values.\n" },
      { version: 2, state: "admitted", description: "Use the shared design tokens for colors, spacing and type.", tags: ["design", "css"], author_bot: "Designer", submitted_at: iso(now() - 86400e3 * 6),
        admitted_by: "Librarian", admitted_at: iso(now() - 86400e3 * 5), scan_verdict: "pass", scanner_version: "1", sha256: "3b7e1c9a52d84f60e1a7c3b8d9f20e56a4c18b7d3e90f2a615c4b8e7d1a03f96",
        content: "---\nname: design-tokens\ndescription: Use the shared design tokens for colors, spacing and type.\n---\n# Design tokens\nUse the tokens for colors, spacing and type.\nNever write raw hex values.\n" },
    ],
    "release-notes": [
      { version: 1, state: "admitted", description: "Write short, user-facing release notes from a list of changes.", tags: ["writing", "release"], author_bot: "Writer", submitted_at: iso(now() - 86400e3 * 9),
        admitted_by: "Librarian", admitted_at: iso(now() - 86400e3 * 8), scan_verdict: "pass", scanner_version: "1", sha256: "c40a9e7d31b58f2a6e0d4c7b19a3f85e2d6b0c48a7f1e93d5b2c6a80f4e1d7b3",
        content: "---\nname: release-notes\ndescription: Write short, user-facing release notes from a list of changes.\n---\n# Release notes\nOne line per change, in the user's words.\n" },
    ],
    "commit-style": [
      { version: 1, state: "admitted", description: "Keep commit messages short and in the imperative.", tags: ["git"], author_bot: "Librarian", submitted_at: iso(now() - 86400e3 * 3),
        admitted_by: "Librarian", admitted_at: iso(now() - 86400e3 * 3), self_admitted: true, scan_verdict: "pass", scanner_version: "1", sha256: "7d1f3a9c5e20b84f6a1d7c3e9b05f2a8c4e6d1b7a30f95c2e8b4d6a1f07c3e59",
        content: "---\nname: commit-style\ndescription: Keep commit messages short and in the imperative.\n---\n# Commit style\nSubject under 60 characters, imperative mood.\n" },
    ],
    "link-checker": [
      { version: 1, state: "proposed", description: "Check the links in a document before it goes out.", tags: ["writing", "qa"], author_bot: "Writer", submitted_at: iso(now() - 3600e3 * 5),
        scan_verdict: "flag", scanner_version: "1", sha256: "e85b2d7a14c9f3601a5e8d2b7c4f90a3d6e1b58c7a2f04d9e3b6a1c85f27d04b",
        scan_hits: [{ id: "net.unlisted-host", line: 6, severity: "flag", excerpt: "Read https://unlisted.example.org/docs first." }],
        content: "---\nname: link-checker\ndescription: Check the links in a document before it goes out.\n---\n# Link checker\nList every link, then test each one.\nRead https://unlisted.example.org/docs first.\n" },
    ],
  },
};
function libSummary(id) {
  const vs = DEMO_LIB.versions[id];
  const latest = Math.max(0, ...vs.filter((v) => v.state === "admitted").map((v) => v.version));
  const shown = vs.find((v) => v.version === latest) || vs[vs.length - 1];
  return { id, description: shown.description, tags: shown.tags, latest_admitted: latest, versions: vs.map((v) => ({ version: v.version, state: v.state })) };
}
function demoLibrary(op, a) {
  const strip = (id, v) => ({ id, ...v, content: undefined, latest_admitted: libSummary(id).latest_admitted });
  if (op === "library.list") {
    const q = (a.query || "").toLowerCase();
    const skills = Object.keys(DEMO_LIB.versions).map(libSummary)
      .filter((s) => (!a.tag || s.tags.includes(a.tag)) && (!q || (s.id + " " + s.description + " " + s.tags.join(" ")).toLowerCase().includes(q)));
    return { skills, curator: DEMO_LIB.curator };
  }
  if (op === "library.queue") {
    return { proposals: Object.entries(DEMO_LIB.versions).flatMap(([id, vs]) => vs.filter((v) => v.state === "proposed").map((v) => strip(id, v))), curator: DEMO_LIB.curator };
  }
  if (op === "library.entry") {
    const vs = DEMO_LIB.versions[a.id];
    if (!vs) throw new Error("no skill " + a.id);
    return { id: a.id, versions: vs.map((v) => strip(a.id, v)), curator: DEMO_LIB.curator };
  }
  if (op === "library.version") {
    const v = (DEMO_LIB.versions[a.id] || []).find((x) => x.version === a.version);
    if (!v) throw new Error("no such version");
    return { id: a.id, ...v };
  }
  if (op === "library.withdraw") return { approval_id: (args.skills || []).length > 1 ? "withdrawmany" : "withdraw", ticket: "demo" };
  if (a.name && a.name.toLowerCase() === DEMO_LIB.curator.toLowerCase()) throw new Error("that is already the curator");
  return { approval_id: "curator", ticket: "demo" };
}

const DEMO_NOTES = [
  { id: "n1", title: "Gift ideas for Anna", body: "Ceramics class\nThe novel she mentioned (ask Bob for the title)", tags: ["birthday"], pinned: true, by: "agent", updated: iso(now() - 3600e3) },
  { id: "n2", title: "Door code for the new office", body: "Ask reception on Monday", tags: ["work"], private: true, by: "you", updated: iso(now() - 86400e3) },
  { id: "n3", title: "Trip to Lisbon", body: "Flights booked. Hotel: check-in after 15:00.\nMuseum tickets for Saturday.", tags: ["travel"], by: "agent", updated: iso(now() - 86400e3 * 3) },
];

// The screens the gallery offers, in the order a new user meets them.
export const DEMO_SCREENS = [
  ["pair", "Set up Rubi"],
  ["unlock", "Unlock"],
  ["send", "Approve an email"],
  ["install", "Install a plugin"],
  ["installmany", "Install several plugins"],
  ["updateall", "Update Rubi and plugins"],
  ["withdrawmany", "Withdraw several skills"],
  ["removemany", "Remove several plugins"],
  ["rollbackmany", "Roll back several plugins"],
  ["reveal", "Choose what to show"],
  ["door", "A sensitive shortcut"],
  ["botpaste", "A Bot asks to connect"],
  ["botaccess", "A Bot asks for access, for good and for a while"],
  ["settings", "Settings"],
  ["approvals", "Approval levels"],
  ["security", "Security"],
  ["store", "Plugin store"],
  ["config", "Plugin settings"],
  ["setup", "Connect with a code"],
  ["home", "Add a home computer"],
  ["bots", "Grok Bot connections"],
  ["library", "Skill library"],
  ["connect", "Connect a Bot"],
  ["updates", "Updates"],
  ["notes", "Notes"],
  ["t3trust", "T3 Agents: trusts"],
  ["t3setup", "T3 Agents: setup"],
  ["t3sessions", "T3 Agents: projects and sessions"],
  ["t3chat", "T3 Agents: a chat"],
  ["t3roles", "T3 Agents: roles"],
  ["t3ssh", "T3 Agents: SSH targets"],
  ["t3usage", "T3 Agents: usage and sign-in"],
  ["sent", "Sent"],
  ["declined", "Declined"],
  ["failed", "It didn't work"],
  ["status", "Status"],
  ["error", "An error"],
];
