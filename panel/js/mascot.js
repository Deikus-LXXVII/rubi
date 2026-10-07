// Rubi's mascot: a coral-red rounded hexagon with two eyes. The eyes show Rubi's mood.
// Kept in one place so the artwork can be refined without touching the screens.

const NS = "http://www.w3.org/2000/svg";

// Body: a hexagon rotated 15°, rounded by a thick stroke in the same color.
const BODY = "84.8,59.3 59.3,84.8 24.5,75.5 15.2,40.7 40.7,15.2 75.5,24.5";

// Eye shapes per mood, drawn in a 100×100 box.
const EYES = {
  idle: [
    { tag: "rect", x: 41, y: 41, width: 7, height: 15, rx: 3.5 },
    { tag: "rect", x: 55, y: 41, width: 7, height: 15, rx: 3.5 },
  ],
  locked: [ // asleep
    { tag: "rect", x: 38, y: 50, width: 11, height: 4, rx: 2 },
    { tag: "rect", x: 54, y: 50, width: 11, height: 4, rx: 2 },
  ],
  happy: [ // ^ ^
    { tag: "path", d: "M39 52 l5.5 -6 l5.5 6", stroke: true },
    { tag: "path", d: "M53 52 l5.5 -6 l5.5 6", stroke: true },
  ],
  alert: [ // wide open
    { tag: "rect", x: 39.5, y: 38, width: 9, height: 19, rx: 4.5 },
    { tag: "rect", x: 54.5, y: 38, width: 9, height: 19, rx: 4.5 },
  ],
};

function el(tag, attrs) {
  const n = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, String(v));
  return n;
}

// mascot returns an <svg> element. mood: idle | locked | happy | alert. size in CSS pixels.
export function mascot(mood = "idle", size = 48) {
  const svg = el("svg", { viewBox: "0 0 100 100", width: size, height: size, class: `mascot mood-${mood}`,
    role: "img", "aria-label": `Rubi (${mood})` });
  svg.append(el("polygon", { points: BODY, class: "mascot-body", "stroke-linejoin": "round", "stroke-width": 14 }));
  const eyes = el("g", { class: "mascot-eyes" });
  for (const e of EYES[mood] || EYES.idle) {
    const { tag, stroke, ...attrs } = e;
    const shape = el(tag, attrs);
    if (stroke) {
      shape.setAttribute("fill", "none");
      shape.setAttribute("stroke-width", "4");
      shape.setAttribute("stroke-linecap", "round");
      shape.setAttribute("stroke-linejoin", "round");
      shape.setAttribute("class", "eye-stroke");
    }
    eyes.append(shape);
  }
  svg.append(eyes);
  return svg;
}
