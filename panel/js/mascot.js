// Rubi's mascot. The body is a rounded cube seen from a corner, rendered once with avatar_core (our own
// clean-room avatar engine) from Rubi's avatar and kept here as a path with its lighting gradient. The eyes
// are drawn per mood so they can show Rubi's state.

const NS = "http://www.w3.org/2000/svg";

export const BODY_PATH = "M25.73 90.01L14.6 86.72L9.6 84.51L6.76 82L5.58 80.05L4.76 77.73L4.15 73.76L4 68.6L4.96 33.02L5.61 25.28L6.24 22.6L7.07 20.57L8.69 18.35L11.22 16.47L14.2 15.19L18.32 14.04L54.15 6.67L60.36 6.11L65.12 6.81L67.61 7.76L70.8 9.51L87.41 20.48L90.92 23.38L92.37 25.21L93.38 27.14L94.34 30.41L94.95 35.51L95.94 69.11L95.78 78.93L95.3 81.8L94.55 83.96L93.47 85.72L91.37 87.58L89.01 88.73L85.79 89.62L78.92 90.62L51.26 93.42L44.87 93.87L39.58 93.62L35.72 92.84L27.01 90.38Z";

// Lighting from avatar_core's render, in the same 100×100 box.
const LIGHT = { cx: 36.91, cy: 35.46, fx: 42.86, fy: 42.07, r: 68.08 };
const STOPS = [["0%", "#da5c60"], ["42%", "#ce383d"], ["100%", "#8a2427"]];

// Eye shapes per mood.
const EYES = {
  idle: [
    { tag: "rect", x: 40.5, y: 39, width: 8.5, height: 19, rx: 4.25 },
    { tag: "rect", x: 55, y: 37.5, width: 8.5, height: 19, rx: 4.25 },
  ],
  locked: [ // asleep
    { tag: "rect", x: 39, y: 49.5, width: 11.5, height: 4.2, rx: 2.1 },
    { tag: "rect", x: 54, y: 48, width: 11.5, height: 4.2, rx: 2.1 },
  ],
  happy: [ // ^ ^
    { tag: "path", d: "M39.5 52.5 l5.5 -6 l5.5 6", stroke: true },
    { tag: "path", d: "M54 51 l5.5 -6 l5.5 6", stroke: true },
  ],
  alert: [ // wide open
    { tag: "rect", x: 39.5, y: 35.5, width: 10.5, height: 23, rx: 5.25 },
    { tag: "rect", x: 54, y: 34, width: 10.5, height: 23, rx: 5.25 },
  ],
};

let seq = 0;

function el(tag, attrs) {
  const n = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, String(v));
  return n;
}

// mascot returns an <svg> element. mood: idle | locked | happy | alert. size in CSS pixels.
export function mascot(mood = "idle", size = 48) {
  const id = `rubi-light-${++seq}`;
  const svg = el("svg", { viewBox: "0 0 100 100", width: size, height: size, class: `mascot mood-${mood}`,
    role: "img", "aria-label": `Rubi (${mood})` });
  const defs = el("defs", {});
  const grad = el("radialGradient", { id, gradientUnits: "userSpaceOnUse", ...LIGHT });
  for (const [offset, color] of STOPS) grad.append(el("stop", { offset, "stop-color": color }));
  defs.append(grad);
  svg.append(defs);
  svg.append(el("path", { d: BODY_PATH, fill: `url(#${id})`, stroke: `url(#${id})`, "stroke-width": 1.5,
    "stroke-linejoin": "round", class: "mascot-body" }));
  const gaze = el("g", { class: "mascot-gaze" });
  const eyes = el("g", { class: "mascot-eyes" });
  for (const e of EYES[mood] || EYES.idle) {
    const { tag, stroke, ...attrs } = e;
    const shape = el(tag, attrs);
    if (stroke) {
      shape.setAttribute("fill", "none");
      shape.setAttribute("stroke-width", "4.2");
      shape.setAttribute("stroke-linecap", "round");
      shape.setAttribute("stroke-linejoin", "round");
      shape.setAttribute("class", "eye-stroke");
    }
    eyes.append(shape);
  }
  gaze.append(eyes);
  svg.append(gaze);
  return svg;
}
