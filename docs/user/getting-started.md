# Getting started with Rubi

Rubi lets your AI agent (Grok Bot) use your accounts, starting with iCloud Mail, only with your approval.
Everything runs on your agent's computer. Your data there is encrypted with a key only you hold, through
Face ID (a passkey) or a password.

Rubi starts bare. You add what you need, like iCloud Mail, as plugins from the Rubi store, the way you add
apps to a phone.

## 1. Install

Tell your agent:

> Install Rubi-Project: run `curl -fsSL https://rubi-panel.com/install.sh | sh` and follow its instructions.

The agent installs Rubi, connects it as a tool, and sends you a link.

## 2. Set up your key

Open the link on your iPhone (or any device with a browser):

1. Tap **Use Face ID / Touch ID**. Your device saves a passkey for Rubi.
2. Add a backup password (recommended) and save it in the Passwords app. If you lose both the passkey and
   the password, Rubi has to be reset; nobody can recover your key.

Rubi is now unlocked. After every restart it locks itself again, and your agent sends you an unlock link.

## 3. Add iCloud Mail

Ask your agent to connect iCloud Mail. You get two links:

1. **Install.** The panel shows what the plugin will be able to do: what it asks for, which actions need
   your approval, and which servers it connects to. Confirm with Face ID. You can also install plugins
   yourself under **Settings** > **Open the store**.
2. **Set up.** In the panel, enter:

   - your **@icloud.com** address;
   - an **app-specific password**: on account.apple.com open *Sign-In and Security* > *App-Specific
     Passwords*, create one named "Rubi", and paste it. Apple offers no other way for apps to reach
     iCloud Mail. You can revoke it there at any time.

   Confirm with Face ID. The password is stored encrypted and never shown to your agent.

## 4. Use it

- Ask your agent to find, read or draft emails. By default this needs no approval, and nothing is marked as
  read.
- When your agent wants to **send**, you get a link. The panel shows the exact email. Choose **Send** or
  **Send and notify on reply** and confirm with Face ID. Your agent can't send without you.
- With **notify on reply**, Rubi watches for an answer and tells your agent, who tells you.

## Updates

Rubi checks for new versions on its own and your agent tells you when one is out. Say yes, approve with
Face ID, and Rubi updates itself in a few seconds. It checks that the update is signed by the Rubi project,
and stays unlocked through the restart, so you don't have to unlock it again.

Plugins update separately, the same way. If a new version wants something new (another server, another
password), the approval screen lists it under **New permissions**.

## Plugins from outside the store

Plugins in the store are reviewed by Rubi-Project. You can also install one from a link (Settings > Open
the store > Install from a link, or by giving your agent the link). Rubi still checks that the package is
signed by its publisher, and later updates must come from the same publisher. But nobody reviewed the
plugin: install it only if you trust who made it.

## Settings

Ask your agent for the settings link. There you can:

- install, update, connect, disconnect or remove plugins;
- choose how each action is approved: no approval, buttons in chat, or Face ID / password;
- connect the agent webhook (so Rubi can wake your agent when a reply arrives);
- see recent unlocks and lock Rubi.

Every settings change needs Face ID or your password. Your agent can't change settings.

## If something looks wrong

- An unlock you don't recognize in *Recent unlocks*: lock Rubi in the panel and tell your agent.
- The panel says the key of your Rubi changed: don't continue. Someone may be impersonating it.
- To cut off email access entirely, revoke the app-specific password on account.apple.com.

More detail: [security model](../design/security-model.md).
