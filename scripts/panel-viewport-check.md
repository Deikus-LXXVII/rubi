# Panel viewport check (manual)

Project rule 1: Rubi is fully usable from a phone, and the design also supports desktop proportions. The Go tests
(`internal/repocheck/panelcss_test.go`, `panelflows_test.go`) refuse the constructs that break a narrow screen, but
only a browser shows the result. Run this check before a release and after any change to `panel/`.

Needs: a browser with device emulation (Chrome, Edge or Safari's Responsive Design Mode), nothing else. No Rubi is
needed: the panel's demo shows every screen with made-up data.

## Start

```bash
python3 -m http.server 8080 --bind 127.0.0.1 --directory panel
```

Open `http://localhost:8080/#demo`. A grid of tiles lists every screen; a tile opens one (`#demo=<name>` opens it
directly, for example `#demo=send`).

## Two viewports

Run the whole list once at each size, with the browser's device toolbar (or Responsive Design Mode) set to:

| Name    | Size      | Notes                                                        |
| ------- | --------- | ------------------------------------------------------------ |
| Phone   | 375 x 812 | touch emulation on; also try 360 x 740 (the narrowest we support) |
| Desktop | 1280 x 800 | mouse                                                        |

## What to look at on every screen

Phone (375 x 812):

1. No horizontal scroll bar on the page, and the page cannot be dragged sideways. Wide text (addresses, keys, long
   summaries) wraps or is cut with an ellipsis inside its box.
2. Every button and field can be tapped with a thumb: at least about 40 px tall, nothing overlapped by the
   fixed save bar or the on-screen keyboard (focus a field and look).
3. The main action of the screen (Approve, Save, Unlock) is reachable without hunting: visible after at most one
   scroll.
4. Text is readable without zooming (body text at least 16 px; no text clipped).
5. Rotate to landscape (812 x 375): still usable.

Desktop (1280 x 800):

6. The layout uses the width sensibly (a card or two columns, not a thin strip glued to the left or stretched to
   the full width).
7. Keyboard: Tab reaches every control in order, and the focus ring is visible.

Both:

8. Dark and light mode (the browser's emulated `prefers-color-scheme`) are both legible.
9. No console errors.

## Screens to open (from the `#demo` grid)

Set up Rubi, Unlock, Approve an email, Install a plugin, Choose what to show, A sensitive shortcut, A Bot asks to
connect (the screen with a pasted webhook), Settings, Approval levels, Security, Plugin store, Plugin settings,
Connect with a code, Add a home computer, Grok Bot connections, Skill library, Connect a Bot, Updates, Notes, Sent,
Declined, It didn't work.

The demo also shows the pages of a plugin (the made-up T3 Agents plugin; it is listed in Settings, Plugins, and each
page has its own tile). Open each at both sizes:

| `#demo=`     | What to look at                                                                                     |
| ------------ | --------------------------------------------------------------------------------------------------- |
| `t3trust`    | The plugin's first page: five link rows, trusted sessions and projects (one UNTIL REVOKED), bound Bots. Long notes wrap. |
| `t3setup`    | Setup rows. Press Set up everything or Build: a Working row with Cancel and progress lines, the page refreshes by itself. Sign in on a role: a link as text, then Open the sign-in page and Paste the code (the question box, with the keyboard up), then done. Run self-test: ok lines. Self-test before any sign-in: a warning row. |
| `t3sessions` | Projects with their sessions, RUNNING now, two warning rows, New session (question box), Manage (a row with up to 8 buttons: they wrap, none is cut off). |
| `t3chat`     | The chat: a Bot's message marked "untrusted text", the Message box above the keyboard, Send, Interrupt. |
| `t3roles`    | Roles of two projects; Open a role: about 20 tool rows, deny rules, SSH grants, History and Show changes. |
| `t3ssh`      | Targets with long host-key lines and a changed-key warning; "Targets being added" with six buttons per row and **Check all host keys**; the public key line (a long unbroken string must wrap or scroll inside its box); Audit. |
| `t3usage`    | Usage and sign-in rows. |

Also check what the demo cannot show, against a real Rubi (see `panel/README.md`, "Local development"):

- an approval with a batch of items (untick one, approve the rest in one go);
- a sign-in step shown by a plugin page with the link opened on the same phone (the demo only shows the page; its link is made up).

## Record

Write the date, the browser and the result in the pull request: "375 x 812: ok, 1280 x 800: ok", or the screen and
the problem. A problem that is a fixed width, a missing `min-width: 0`, or a `white-space: nowrap` should become a
rule in `panelcss_test.go` so it cannot come back.
