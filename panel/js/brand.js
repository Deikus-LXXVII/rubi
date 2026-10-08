// The living brand book: renders the real mascot, tokens, components and icons (see brand.html).

import { ICONS, icon } from "./icons.js";
import { MOODS, mascot, react, setMood } from "./mascot.js";

const $ = (id) => document.getElementById(id);

function h(tag, attrs = {}, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else if (k === "style") el.style.cssText = v;
    else el.setAttribute(k, v);
  }
  el.append(...kids.flat().filter((x) => x != null));
  return el;
}

// Mascot: every mood, and the reactions on a big Rubi.
const moods = $("moods");
for (const m of MOODS) moods.append(h("figure", {}, mascot(m, 110), h("figcaption", {}, m)));
const big = mascot("idle", 140);
const reactions = $("reactions");
reactions.append(big);
for (const r of ["pop", "hop", "nod", "sigh", "shake"]) {
  reactions.append(h("button", { class: "secondary small", onclick: () => react(big, r) }, r));
}
const moodSel = h("select", { onchange: (e) => setMood(big, e.target.value), "aria-label": "Mood" }, MOODS.map((m) => h("option", { value: m }, m)));
reactions.append(moodSel);

// Colour: read the live tokens.
const css = getComputedStyle(document.documentElement);
const groups = [
  ["Ruby", ["--ruby-300", "--ruby-400", "--ruby-500", "--ruby-600", "--ruby-800"]],
  ["Ink", ["--bg", "--bg-2", "--card", "--raise", "--line-2", "--muted", "--text"]],
  ["Signal", ["--ok", "--warn", "--danger"]],
];
for (const [name, tokens] of groups) {
  $("swatches").append(h("div", { class: "bb-swatch-group" }, h("h3", {}, name),
    h("div", { class: "bb-swatch-row" }, tokens.map((t) => h("div", { class: "bb-swatch" },
      h("span", { style: `background: var(${t})` }), h("code", {}, t), h("small", {}, css.getPropertyValue(t).trim()))))));
}

// Motion: each curve moves a dot when pressed.
const curves = [
  ["--ease-out", "Arriving"], ["--ease-in", "Leaving"], ["--ease-in-out", "Moving in place"],
];
for (const [token, use] of curves) {
  const dot = h("i", { class: "bb-dot" });
  const track = h("div", { class: "bb-track" }, dot);
  const row = h("button", { class: "bb-curve", type: "button", onclick: () => {
    dot.style.transition = "none";
    dot.style.translate = "0";
    requestAnimationFrame(() => requestAnimationFrame(() => {
      dot.style.transition = `translate 900ms var(${token})`;
      dot.style.translate = "calc(100% * 0 + var(--run))";
    }));
  } }, h("span", {}, h("code", {}, token), h("small", {}, use)), track);
  $("curves").append(row);
}
$("curves").append(h("p", { class: "muted small" }, "Durations: 90 ms press · 160 ms hover · 240 ms appear · 420 ms screen change · 700 ms celebration."));

// Components.
const sw = h("input", { type: "checkbox", role: "switch", class: "switch", checked: true, "aria-label": "Example switch" });
const busyBtn = h("button", { class: "primary", onclick: () => {
  busyBtn.classList.add("is-busy");
  setTimeout(() => busyBtn.classList.remove("is-busy"), 1600);
} }, icon("passkey", 20), "Approve with Passkey");
$("components-box").append(
  h("div", { class: "bb-col" }, busyBtn, h("button", { class: "secondary" }, "Secondary"), h("button", { class: "danger" }, "Remove plugin"), h("button", { class: "link" }, "Link")),
  h("div", { class: "bb-col" },
    h("input", { placeholder: "Your Rubi password", type: "password" }),
    h("label", { class: "switch-row" }, h("span", {}, "Notify me when Anna replies"), sw),
    h("div", {}, h("span", { class: "tag" }, "Reviewed"), " ", h("span", { class: "tag warn" }, "Update available")),
    h("p", { class: "error", role: "alert" }, "Wrong password. Try again, or use your passkey.")),
);

// Icons.
for (const n of ICONS) $("icon-grid").append(h("figure", {}, icon(n, 26), h("figcaption", {}, n)));
