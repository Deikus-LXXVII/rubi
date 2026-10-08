// Rubi's mascot: a softly rounded hexagon (the silhouette rendered by avatar_core, our own clean-room
// avatar engine), lit from the top left, with two eyes that carry the emotion. Everything is drawn here as
// SVG so it can move: it breathes, blinks at natural intervals, glances around, changes mood smoothly and
// reacts (hop, nod, sigh, shake). See docs/brand/brand-book.md, section 3.

const NS = "http://www.w3.org/2000/svg";

export const BODY_PATH = "M57.39 91.05L58.65 90.43L59.82 89.8L61.25 89L63.26 87.83L77.45 79.4L80.26 77.69L81.97 76.59L83.28 75.69L84.48 74.78L85.6 73.81L86.1 73.33L87.01 72.32L87.8 71.24L88.49 70.07L89.04 68.89L89.28 68.25L89.5 67.56L89.71 66.83L89.9 66.04L90.2 64.4L90.34 63.45L90.59 61.06L90.78 58.4L90.97 55.24L91.78 40.34L91.99 35.94L92.06 33.49L92.06 31.66L92.03 30.63L91.91 28.87L91.81 28.08L91.69 27.35L91.47 26.27L91.18 25.29L90.85 24.39L90.46 23.56L89.93 22.62L89.31 21.77L88.62 20.98L87.86 20.24L87.24 19.73L86.59 19.24L85.87 18.75L85.11 18.28L84.28 17.81L83.38 17.34L82.04 16.68L80.41 15.94L78.14 14.97L62.09 8.33L59.27 7.2L57.78 6.64L56.58 6.23L55.55 5.9L54.62 5.64L53.76 5.43L52.96 5.27L52.19 5.15L51.45 5.07L50.72 5.02L50 5L49.28 5.02L48.55 5.07L47.81 5.15L47.04 5.27L46.24 5.43L45.38 5.64L44.45 5.9L43.42 6.23L41.52 6.9L37.91 8.33L26.12 13.19L21.86 14.97L18.73 16.33L17.26 17.02L15.72 17.81L14.89 18.28L14.13 18.75L13.41 19.24L12.76 19.73L12.14 20.24L11.38 20.98L10.69 21.77L10.07 22.62L9.54 23.56L9.15 24.39L8.82 25.29L8.53 26.27L8.31 27.35L8.19 28.08L8.09 28.87L7.97 30.63L7.94 31.66L7.95 34.21L8.12 38.4L9.09 56.28L9.29 59.43L9.41 61.06L9.66 63.45L9.8 64.4L10.1 66.04L10.29 66.83L10.5 67.56L10.72 68.25L10.96 68.89L11.51 70.07L12.2 71.24L12.99 72.32L13.9 73.33L14.4 73.81L15.52 74.78L16.72 75.69L18.03 76.59L19.74 77.69L22.55 79.4L34.61 86.58L37.85 88.48L40.18 89.8L42.01 90.76L43.76 91.57L44.79 91.98L45.75 92.31L46.65 92.57L47.52 92.76L48.36 92.89L49.18 92.97L50 93L50.82 92.97L51.64 92.89L52.48 92.76L53.35 92.57L54.25 92.31L55.21 91.98L56.24 91.57Z";

// The eyes sit a little right of centre: Rubi is seen slightly from its left, as in the avatar render.
const EYES = [
  { cx: 42, cy: 55, tilt: -5 },
  { cx: 61, cy: 53.6, tilt: 5 },
];
const EYE_W = 8.4;
const EYE_H = 19;

export const MOODS = ["idle", "locked", "thinking", "happy", "alert", "concern"];

const reduced = () => matchMedia("(prefers-reduced-motion: reduce)").matches;
let seq = 0;

function el(tag, attrs = {}, ...kids) {
  const n = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, String(v));
  n.append(...kids);
  return n;
}

function stops(...list) {
  return list.map(([offset, color, opacity = 1]) => el("stop", { offset, "stop-color": color, "stop-opacity": opacity }));
}

// mascot returns Rubi as an <svg>. mood: one of MOODS. size in CSS pixels. opts.follow makes the eyes
// (and a little of the body) follow the pointer, for the landing page.
export function mascot(mood = "idle", size = 48, opts = {}) {
  const id = `rubi${++seq}`;
  const svg = el("svg", { viewBox: "0 0 100 100", width: size, height: size, class: `mascot mood-${mood}`,
    role: "img", "aria-label": `Rubi (${mood})`, "data-mood": mood });
  if (size < 34) svg.classList.add("tiny"); // icons: no shadow or highlights, crisper eyes

  svg.append(el("defs", {},
    el("radialGradient", { id: `${id}-body`, cx: "34%", cy: "26%", r: "82%", fx: "30%", fy: "20%" },
      ...stops(["0%", "#f2898b"], ["22%", "#e2585c"], ["52%", "#ce383d"], ["82%", "#a12b2f"], ["100%", "#7c1f23"])),
    el("linearGradient", { id: `${id}-rim`, x1: "0", y1: "0", x2: "0", y2: "1" },
      ...stops(["0%", "#ffffff", 0.55], ["38%", "#ffffff", 0], ["86%", "#000000", 0], ["100%", "#000000", 0.28])),
    el("radialGradient", { id: `${id}-spec`, cx: "50%", cy: "50%", r: "50%" },
      ...stops(["0%", "#ffffff", 0.55], ["100%", "#ffffff", 0])),
    el("radialGradient", { id: `${id}-floor`, cx: "50%", cy: "50%", r: "50%" },
      ...stops(["0%", "#000000", 0.42], ["100%", "#000000", 0])),
    el("clipPath", { id: `${id}-clip` }, el("path", { d: BODY_PATH })),
  ));

  svg.append(el("ellipse", { class: "mascot-floor", cx: 50, cy: 95.5, rx: 30, ry: 3.4, fill: `url(#${id}-floor)` }));
  const body = el("g", { class: "mascot-body" });
  body.append(
    el("path", { class: "mascot-skin", d: BODY_PATH, fill: `url(#${id}-body)` }),
    el("g", { "clip-path": `url(#${id}-clip)` },
      el("ellipse", { class: "mascot-spec", cx: 30, cy: 22, rx: 17, ry: 9, transform: "rotate(-24 30 22)", fill: `url(#${id}-spec)` })),
    el("path", { class: "mascot-rim", d: BODY_PATH, fill: "none", stroke: `url(#${id}-rim)`, "stroke-width": 1.4 }),
  );

  const gaze = el("g", { class: "mascot-gaze" });
  const blinkers = el("g", { class: "mascot-eyes" });
  EYES.forEach((e, i) => {
    const eye = el("g", { class: `eye eye-${i ? "r" : "l"}`, transform: `rotate(${e.tilt} ${e.cx} ${e.cy})` });
    // Each mood is a shape; moods cross-fade and morph through CSS, so changes are smooth.
    eye.append(
      el("rect", { class: "eye-pill", x: e.cx - EYE_W / 2, y: e.cy - EYE_H / 2, width: EYE_W, height: EYE_H, rx: EYE_W / 2 }),
      el("path", { class: "eye-arc eye-stroke", d: `M${e.cx - 5} ${e.cy + 2.5} q5 -7.5 10 0`, fill: "none",
        "stroke-width": 3.8, "stroke-linecap": "round" }),
      el("path", { class: "eye-lid eye-stroke", d: `M${e.cx - 5.5} ${e.cy} q5.5 5 11 0`, fill: "none",
        "stroke-width": 3.4, "stroke-linecap": "round" }),
      el("circle", { class: "eye-glint", cx: e.cx - 1.3, cy: e.cy - 5.2, r: 1.45 }),
    );
    blinkers.append(eye);
  });
  gaze.append(blinkers);
  body.append(gaze);
  svg.append(body);

  if (opts.follow) follow(svg);
  life.add(svg);
  wake();
  return svg;
}

// setMood changes Rubi's mood in place, with the eyes morphing to the new shape.
export function setMood(svg, mood) {
  if (!svg) return;
  svg.classList.remove(...MOODS.map((m) => `mood-${m}`));
  svg.classList.add(`mood-${mood}`);
  svg.dataset.mood = mood;
  svg.setAttribute("aria-label", `Rubi (${mood})`);
}

// ---------- reactions ----------

const SPRING = "linear(0, 0.009, 0.035 2.1%, 0.141 4.4%, 0.723 12.9%, 0.938 16.7%, 1.017 19.4%, 1.067, 1.099 24.3%, 1.108 26%, 1.103, 1.085 31.4%, 1.019 39.1%, 0.998 43.5%, 0.991 47.6%, 1.001 62.4%, 1)";

const REACTIONS = {
  hop: {
    body: [
      { transform: "translateY(0) scale(1, 1)" },
      { transform: "translateY(2px) scale(1.08, 0.9)", offset: 0.16 },
      { transform: "translateY(-16%) scale(0.95, 1.07)", offset: 0.45 },
      { transform: "translateY(0) scale(1.07, 0.92)", offset: 0.78 },
      { transform: "translateY(0) scale(1, 1)" },
    ],
    floor: [
      { transform: "scale(1)", opacity: 1 },
      { transform: "scale(1.08)", opacity: 1, offset: 0.16 },
      { transform: "scale(0.62)", opacity: 0.55, offset: 0.45 },
      { transform: "scale(1.06)", opacity: 1, offset: 0.78 },
      { transform: "scale(1)", opacity: 1 },
    ],
    duration: 760,
    easing: "cubic-bezier(.45, 0, .55, 1)",
  },
  nod: {
    body: [
      { transform: "none" },
      { transform: "translateY(3%) rotate(4deg) scale(1.02, 0.97)", offset: 0.3 },
      { transform: "translateY(-1%) rotate(-3deg)", offset: 0.65 },
      { transform: "none" },
    ],
    duration: 620,
    easing: "cubic-bezier(.65, 0, .35, 1)",
  },
  sigh: {
    body: [
      { transform: "none" },
      { transform: "translateY(5%) scale(1.05, 0.92)", offset: 0.5 },
      { transform: "translateY(2%) scale(1.02, 0.97)" },
    ],
    duration: 1200,
    easing: "cubic-bezier(.65, 0, .35, 1)",
    fill: "forwards",
  },
  shake: {
    body: [
      { transform: "none" },
      { transform: "translateX(-4%) rotate(-5deg)", offset: 0.2 },
      { transform: "translateX(4%) rotate(5deg)", offset: 0.45 },
      { transform: "translateX(-2%) rotate(-2deg)", offset: 0.7 },
      { transform: "none" },
    ],
    duration: 560,
    easing: "cubic-bezier(.65, 0, .35, 1)",
  },
  pop: {
    body: [
      { transform: "scale(0.4) rotate(-12deg)", opacity: 0 },
      { transform: "scale(1) rotate(0)", opacity: 1 },
    ],
    floor: [{ transform: "scale(0.3)", opacity: 0 }, { transform: "scale(1)", opacity: 1 }],
    duration: 760,
    easing: SPRING,
  },
};

// react plays one reaction. It returns a promise that settles when it ends.
export function react(svg, kind, delay = 0) {
  const r = REACTIONS[kind];
  if (!svg || !r || !svg.animate) return Promise.resolve();
  if (reduced() && kind !== "pop") return Promise.resolve();
  const body = svg.querySelector(".mascot-body");
  const floor = svg.querySelector(".mascot-floor");
  const opts = { duration: r.duration, easing: r.easing, delay, fill: r.fill || "both" };
  const anims = [body.animate(r.body, opts)];
  if (r.floor) anims.push(floor.animate(r.floor, opts));
  return Promise.all(anims.map((a) => a.finished.catch(() => {})));
}

// ---------- life: blinking and glancing, for every mascot on the page ----------

const life = new Set();
let timer = null;

function wake() {
  if (timer) return;
  timer = setTimeout(tick, 1200 + Math.random() * 1500);
}

function tick() {
  timer = null;
  for (const svg of [...life]) {
    if (!svg.isConnected) {
      life.delete(svg);
      continue;
    }
    const mood = svg.dataset.mood;
    if (mood === "locked" || mood === "happy") continue;
    if (Math.random() < 0.7) blink(svg, Math.random() < 0.18);
    else if (mood === "idle" && !svg.classList.contains("following") && !reduced()) glance(svg);
  }
  if (life.size) timer = setTimeout(tick, 2200 + Math.random() * 3400);
}

function blink(svg, twice) {
  const eyes = svg.querySelector(".mascot-eyes");
  if (!eyes?.animate) return;
  const close = [
    { transform: "scaleY(1)" },
    { transform: "scaleY(0.08)", offset: 0.45 },
    { transform: "scaleY(1)" },
  ];
  eyes.animate(close, { duration: 170, easing: "cubic-bezier(.45, 0, .55, 1)" });
  if (twice) eyes.animate(close, { duration: 170, delay: 240, easing: "cubic-bezier(.45, 0, .55, 1)" });
}

function glance(svg) {
  const gaze = svg.querySelector(".mascot-gaze");
  if (!gaze?.animate) return;
  const dx = (Math.random() < 0.5 ? -1 : 1) * (2 + Math.random() * 2);
  const dy = Math.random() * 2 - 1.4;
  gaze.animate([
    { transform: "translate(0, 0)" },
    { transform: `translate(${dx}px, ${dy}px)`, offset: 0.18 },
    { transform: `translate(${dx}px, ${dy}px)`, offset: 0.78 },
    { transform: "translate(0, 0)" },
  ], { duration: 1900, easing: "cubic-bezier(.65, 0, .35, 1)" });
}

// follow makes the eyes look toward the pointer and the body lean a little.
function follow(svg) {
  svg.classList.add("following");
  let raf = 0;
  let target = { x: 0, y: 0 };
  const cur = { x: 0, y: 0 };
  const step = () => {
    cur.x += (target.x - cur.x) * 0.12;
    cur.y += (target.y - cur.y) * 0.12;
    svg.style.setProperty("--gx", `${(cur.x * 4).toFixed(2)}px`);
    svg.style.setProperty("--gy", `${(cur.y * 3).toFixed(2)}px`);
    svg.style.setProperty("--lean", `${(cur.x * 4).toFixed(2)}deg`);
    raf = Math.abs(target.x - cur.x) + Math.abs(target.y - cur.y) > 0.002 ? requestAnimationFrame(step) : 0;
  };
  addEventListener("pointermove", (e) => {
    if (reduced() || !svg.isConnected) return;
    const r = svg.getBoundingClientRect();
    const dx = e.clientX - (r.left + r.width / 2);
    const dy = e.clientY - (r.top + r.height / 2);
    const d = Math.hypot(dx, dy) || 1;
    const k = Math.min(1, d / 420);
    target = { x: (dx / d) * k, y: (dy / d) * k };
    if (!raf) raf = requestAnimationFrame(step);
  }, { passive: true });
}
