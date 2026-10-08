// Rubi's icons: 24-unit grid, 1.75 stroke, round caps and joins, drawn in currentColor. Our own drawings,
// kept few and plain (docs/brand/brand-book.md).

const NS = "http://www.w3.org/2000/svg";

const PATHS = {
  passkey: ["M9 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z", "M3 20c0-3.3 2.7-6 6-6 1.2 0 2.3.3 3.2.9", "M17 13.5a2.5 2.5 0 1 0 0 5", "M17 18.5v3l1.5-1.2L20 21.5v-3", "M17 13.5a2.5 2.5 0 0 1 0 5"],
  key: ["M14.5 9.5a4.5 4.5 0 1 1-1.3-3.2", "M13.2 6.3 21 14v3h-3v-2h-2v-2h-2", "M7.5 9.5h.01"],
  lock: ["M6 11h12v9H6z", "M8.5 11V8a3.5 3.5 0 0 1 7 0v3", "M12 15v2"],
  unlock: ["M6 11h12v9H6z", "M8.5 11V8a3.5 3.5 0 0 1 6.8-1.2", "M12 15v2"],
  check: ["M5 12.5 10 17.5 19 7"],
  close: ["M6 6l12 12", "M18 6 6 18"],
  mail: ["M4 6h16v12H4z", "m4 7 8 6 8-6"],
  plug: ["M9 3v5", "M15 3v5", "M6 8h12v3a6 6 0 0 1-12 0z", "M12 17v4"],
  globe: ["M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z", "M3 12h18", "M12 3c2.5 2.8 3.7 5.8 3.7 9s-1.2 6.2-3.7 9c-2.5-2.8-3.7-5.8-3.7-9S9.5 5.8 12 3Z"],
  bell: ["M6 16V11a6 6 0 0 1 12 0v5l1.5 2h-15z", "M10 20.5a2.2 2.2 0 0 0 4 0"],
  shield: ["M12 3 5 6v5.5c0 4.2 3 7.8 7 9.5 4-1.7 7-5.3 7-9.5V6z", "m9 12 2 2 4-4"],
  home: ["M4 11 12 4l8 7", "M6 9.5V20h12V9.5", "M10 20v-5h4v5"],
  clock: ["M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z", "M12 7v5l3 2"],
  warn: ["M12 4 2.8 19.5h18.4z", "M12 10v4", "M12 17h.01"],
  copy: ["M9 9h11v11H9z", "M5 15H4V4h11v1"],
  back: ["M15 5 8 12l7 7"],
  chevron: ["m9 5 7 7-7 7"],
  gear: ["M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z", "M19.4 13.5a7.6 7.6 0 0 0 0-3l2-1.5-2-3.4-2.3.9a7.7 7.7 0 0 0-2.6-1.5L14 2.5h-4l-.5 2.5a7.7 7.7 0 0 0-2.6 1.5l-2.3-.9-2 3.4 2 1.5a7.6 7.6 0 0 0 0 3l-2 1.5 2 3.4 2.3-.9a7.7 7.7 0 0 0 2.6 1.5l.5 2.5h4l.5-2.5a7.7 7.7 0 0 0 2.6-1.5l2.3.9 2-3.4z"],
  user: ["M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z", "M4.5 20c.8-3.5 3.8-6 7.5-6s6.7 2.5 7.5 6"],
  bot: ["M6 9h12v10H6z", "M12 5v4", "M12 5h.01", "M9.5 13.5h.01", "M14.5 13.5h.01", "M3.5 13v3", "M20.5 13v3"],
  sparkle: ["M12 3.5 13.8 9 19.5 10.8 13.8 12.6 12 18.5 10.2 12.6 4.5 10.8 10.2 9z", "M19 3v3", "M17.5 4.5h3"],
  download: ["M12 4v11", "m7.5 10.5 4.5 4.5 4.5-4.5", "M5 20h14"],
  eye: ["M2.5 12s3.5-6.5 9.5-6.5 9.5 6.5 9.5 6.5-3.5 6.5-9.5 6.5S2.5 12 2.5 12Z", "M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z"],
  folder: ["M3.5 6.5h6l2 2h9v10h-17z"],
  undo: ["M9 14 4 9l5-5", "M4 9h10a6 6 0 0 1 0 12h-3"],
  trash: ["M5 7h14", "M10 7V4.5h4V7", "M6.5 7l1 13h9l1-13"],
  plus: ["M12 5v14", "M5 12h14"],
  link: ["M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1", "M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1"],
};

// icon returns an inline SVG icon. size in CSS pixels.
export function icon(name, size = 18, cls = "") {
  const svg = document.createElementNS(NS, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("width", String(size));
  svg.setAttribute("height", String(size));
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("class", `icon icon-${name}${cls ? " " + cls : ""}`);
  for (const d of PATHS[name] || []) {
    const p = document.createElementNS(NS, "path");
    p.setAttribute("d", d);
    svg.append(p);
  }
  return svg;
}

export const ICONS = Object.keys(PATHS);
