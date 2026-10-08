# Rubi brand book

The rules every Rubi surface follows: the panel, the landing page, the README art and the plugins' screens.
The tokens live in `panel/styles.css` (`:root`); the living version of this book is `panel/brand.html`.

## 1. Who Rubi is

Rubi is a small guardian that holds your keys while your AI agent works. The agent does things; Rubi
makes sure nothing that matters happens without you.

- **Promise:** your agent acts, you approve.
- **Character:** calm, attentive, loyal, a little playful. Never sly, never pushy, never alarmist.
- **Shape:** a softly rounded hexagon, a cut gem and a shield at once. Ruby red, lit from the top left.
- **What Rubi is not:** a robot, a lock icon, a mascot that jokes during serious moments.

## 2. Voice

- Plain words, short sentences, second person ("you", "your agent").
- Say what will happen before it happens, and what happened after: "Nothing happens until you approve."
- Name things the way the user sees them: "Send this email?", not "Approve action icloud-mail.send".
- Serious moments get serious words: a warning says what is wrong and what to do, no mascot jokes.
- No emoji in buttons or labels. Buttons are verbs: "Send", "Approve", "Add a home computer".
- English in product files. Numbers and times in the user's locale.

## 3. Mascot

Rubi's body is the rounded hexagon from avatar_core (our own engine), lit with a radial gradient. The
face is two dark eyes; the eyes carry nearly all emotion.

| Mood | When | Eyes | Body |
|---|---|---|---|
| idle | waiting, settings | upright pills, blink every few seconds, glance aside | slow breathing |
| locked | Rubi is locked | closed, gentle curves | slower, deeper breathing, slight droop |
| thinking | connecting, working | small, looking up and around | gentle sway |
| happy | approved, connected | ^ ^ arcs | a band of light sweeps across the body, like a cut ruby turning; a slight lift |
| alert | something needs attention | tall, wide open | quick double nudge |
| concern | an error | slanted, a little lower | small sigh (sink and recover) |

Attention (the mascot shows it is paying attention to the user, not to itself):

- While a password is typed, Rubi closes its eyes and turns a little away.
- Pointing at the primary button makes Rubi look at it.
- On the landing page Rubi follows the pointer and smiles with a shine when clicked.
- Between screens Rubi flies from its old place and size to the new one, then lands with a small squash.

Rules:

- One mascot per screen, at the top, at most 120 px; 20–32 px in headers.
- The mascot reacts to the user's own actions (approving, toggling a notification), never by itself in
  the middle of reading.
- Clear space around it: a quarter of its size. Never stretch, recolour or outline it.
- Never show it next to an impersonation or security warning ("Stop" screens): those show only text.

## 4. Colour

Ruby is the only accent. Everything else is warm ink, so red always means "Rubi" or "act here".

| Token | Dark | Light | Use |
|---|---|---|---|
| `--ruby-300` | #f08a8c | #e2585c | links, focus in dark |
| `--ruby-400` | #e2585c | #ce383d | hover of primary |
| `--ruby-500` | #ce383d | #ce383d | primary buttons, mascot |
| `--ruby-600` | #b32d32 | #b92e33 | pressed primary |
| `--ruby-800` | #8a2427 | #8a2427 | mascot shadow side |
| `--bg` | #0f0d0d | #faf7f5 | page |
| `--bg-2` | #161313 | #f3eeea | wells, code |
| `--card` | #1b1717 | #ffffff | cards |
| `--line` | #2d2726 | #ebe3de | borders |
| `--text` | #f5f1ee | #1b1716 | text |
| `--muted` | #a59d97 | #6f6762 | secondary text |
| `--ok` | #7fd18b | #2e7d32 | success |
| `--warn` | #f2b45a | #9a6200 | caution |
| `--danger` | #ff7b72 | #b3261e | errors, destructive |

- Text contrast at least WCAG AA (4.5:1; 3:1 for large text and icons).
- Danger red is cooler and lighter than ruby, so an error never looks like a primary button.
- The ruby glow (`--glow`) is used only behind the mascot and on the primary action in focus.

## 5. Type

The system typeface (SF Pro on Apple devices, Segoe UI, Roboto): it is what the user's phone already
speaks, loads instantly and needs no font file. Monospace: SF Mono / Menlo for commands and codes.

| Role | Size / line | Weight | Tracking |
|---|---|---|---|
| Display (landing) | clamp(2.4rem, 6vw, 4rem) / 1.04 | 750 | -0.035em |
| H1 | 1.75rem / 1.15 | 700 | -0.025em |
| H2 | 1.15rem / 1.3 | 650 | -0.015em |
| Body | 1rem / 1.55 | 400 | 0 |
| Small | 0.875rem / 1.45 | 450 | 0 |
| Eyebrow | 0.75rem / 1.2 | 650 | 0.08em, uppercase |
| Mono | 0.875rem / 1.5 | 450 | 0 |

## 6. Space, shape, depth

- 4 px grid: 4, 8, 12, 16, 20, 24, 32, 40, 56, 80.
- Radii: 8 (chips), 12 (inputs, small cards), 16 (buttons), 22 (cards), pill (switches, tags).
- Cards sit on the page with a 1 px line and a soft, warm shadow; nothing floats without a reason.
- Touch targets at least 44 × 44 px. Content width: 560 px for focused tasks, 1040 px for settings.

## 7. Motion

Motion explains what happened and where things went. It is quick, soft and never in the way.

**Durations:** `--t-instant` 90 ms (press), `--t-quick` 160 ms (hover, toggles), `--t-base` 240 ms
(appear), `--t-gentle` 420 ms (screen change, expand), `--t-slow` 700 ms (celebration), loops 4–10 s.

**Curves:**

- `--ease-out` cubic-bezier(.16, 1, .3, 1): things arriving.
- `--ease-in` cubic-bezier(.7, 0, .84, 0): things leaving (shorter than arriving).
- `--ease-in-out` cubic-bezier(.65, 0, .35, 1): moving in place.
- No bounce: transitions never overshoot. Even Rubi's entrances and the switch knob use `--ease-out`;
  nothing jumps.

**Patterns:**

- Screen change: the old card fades and drops 8 px (160 ms), the new one rises 16 px with a slight blur
  clearing (420 ms), its blocks staggered 40 ms apart.
- Press: buttons scale to 0.97 on press and ease back.
- Waiting: the button keeps its size and shows three soft dots; after 1.5 s the mascot starts "thinking".
- Success: Rubi smiles (^ ^) and a band of light sweeps across it like a ruby catching the light (no
  jump); hexagon confetti and a ring of ruby light; the title rises.
- Decline / expired: the mascot sighs; no sparks.
- Error: the message slides down; the field shakes twice, small (6 px).
- Expand / collapse: height and opacity together, 420 ms.
- Ambient: the mascot breathes and blinks; nothing else moves on its own (the landing page's aurora
  drifts very slowly).
- Waiting: a thin ruby line runs along the top of the page while Rubi takes more than 250 ms; the first
  connection shows a thinking mascot and a shimmering outline of the card.
- Toggles and checks: the switch knob, radio dot and check mark ease in; the segmented control's
  highlight slides to the chosen option.
- Reduced motion: every animation becomes a short fade; the mascot keeps blinking only.

## 8. UX principles

1. **One clear next step.** Each screen has one primary button; everything else is secondary or a link.
2. **Show the real thing.** Approvals show the exact email, the exact plugin permissions, the exact
   change, the way the user would see it elsewhere.
3. **Say the consequence.** Next to every irreversible action: what happens, and that nothing happens
   until the user approves.
4. **Answer within 100 ms.** Every tap gets immediate feedback; anything longer shows progress.
5. **Errors are instructions.** "Wrong password" plus what to try; never a code or a stack trace.
6. **Safe by default.** Destructive actions are secondary, red only in text, and always ask.
7. **Accessible.** Keyboard reachable, visible focus, labels on every control, AA contrast, no
   information carried by colour or motion alone.
8. **Calm.** No badges that shout, no fake urgency; a countdown appears only when a request really expires.
9. **Try before trusting.** The demo (`#demo`) is the real panel with made-up data, one tap from the landing page.
