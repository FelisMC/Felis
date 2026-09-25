// Console formatting codes. Minecraft servers and plugins colour text with §
// codes (§a green, §l bold, §r reset, and the §x§r§r§g§g§b§b hex form), and a
// server started with a colour terminal writes ANSI SGR escapes instead. Shown
// raw, both turn a log into noise. parseFormatting splits a line into styled
// segments once, when the line arrives; the console renders each segment as a
// React text node inside a styled span, so nothing from the log ever reaches
// the page as markup.

export interface SegmentStyle {
  /** A CSS colour, e.g. "#55ff55". */
  color?: string;
  bold?: boolean;
  italic?: boolean;
  underline?: boolean;
  strike?: boolean;
}

export interface Segment extends SegmentStyle {
  text: string;
}

export interface Formatted {
  /** The line with every code removed: what the level check and copy see. */
  text: string;
  /** Styled runs of text, absent when the line carries no styling. */
  segments?: Segment[];
}

// The Java Edition palette, with the two darkest colours lifted so they stay
// readable on the console's black background.
const MC_COLORS: Record<string, string> = {
  "0": "#71717a",
  "1": "#5c7cfa",
  "2": "#00aa00",
  "3": "#00aaaa",
  "4": "#aa0000",
  "5": "#aa00aa",
  "6": "#ffaa00",
  "7": "#aaaaaa",
  "8": "#555555",
  "9": "#5555ff",
  a: "#55ff55",
  b: "#55ffff",
  c: "#ff5555",
  d: "#ff55ff",
  e: "#ffff55",
  f: "#ffffff",
};

// ANSI 30–37 and 90–97, in the same order as SGR numbers them; black is lifted
// like §0.
const ANSI_COLORS = [
  "#71717a", "#cd3131", "#0dbc79", "#e5e510", "#2472c8", "#bc3fbc", "#11a8cd", "#e5e5e5",
  "#8a8a8a", "#f14c4c", "#23d18b", "#f5f543", "#3b8eea", "#d670d6", "#29b8db", "#ffffff",
];

const hex2 = (n: number) => n.toString(16).padStart(2, "0");
const rgb = (r: number, g: number, b: number) => `#${hex2(r)}${hex2(g)}${hex2(b)}`;

// ansi256 maps a 256-colour index: the 16 base colours, a 6×6×6 cube, then 24
// greys.
function ansi256(n: number): string | undefined {
  if (!Number.isInteger(n) || n < 0 || n > 255) return undefined;
  if (n < 16) return ANSI_COLORS[n];
  if (n < 232) {
    const c = n - 16;
    const level = (v: number) => (v === 0 ? 0 : 55 + v * 40);
    return rgb(level(Math.floor(c / 36)), level(Math.floor(c / 6) % 6), level(c % 6));
  }
  const grey = 8 + (n - 232) * 10;
  return rgb(grey, grey, grey);
}

const byte = (s: string | undefined) => {
  const n = Number(s);
  return Number.isInteger(n) && n >= 0 && n <= 255 ? n : undefined;
};

// applySgr folds one SGR parameter list into the running style. Background
// colours and anything else a log has no use for are skipped.
function applySgr(style: SegmentStyle, params: string): SegmentStyle {
  const p = params === "" ? ["0"] : params.split(";");
  let s = { ...style };
  for (let i = 0; i < p.length; i++) {
    const n = Number(p[i] || "0");
    if (n === 0) s = {};
    else if (n === 1) s.bold = true;
    else if (n === 3) s.italic = true;
    else if (n === 4) s.underline = true;
    else if (n === 9) s.strike = true;
    else if (n === 22) s.bold = undefined;
    else if (n === 23) s.italic = undefined;
    else if (n === 24) s.underline = undefined;
    else if (n === 29) s.strike = undefined;
    else if (n >= 30 && n <= 37) s.color = ANSI_COLORS[n - 30];
    else if (n >= 90 && n <= 97) s.color = ANSI_COLORS[n - 90 + 8];
    else if (n === 39) s.color = undefined;
    else if (n === 38 || n === 48) {
      // Extended colour: 5;n or 2;r;g;b. Its arguments are consumed either
      // way, so they are never read as codes of their own.
      let color: string | undefined;
      if (p[i + 1] === "5") {
        color = ansi256(Number(p[i + 2]));
        i += 2;
      } else if (p[i + 1] === "2") {
        const [r, g, b] = [byte(p[i + 2]), byte(p[i + 3]), byte(p[i + 4])];
        if (r !== undefined && g !== undefined && b !== undefined) color = rgb(r, g, b);
        i += 4;
      }
      if (n === 38 && color) s.color = color;
    }
  }
  return s;
}

const styled = (s: SegmentStyle) =>
  s.color !== undefined || s.bold || s.italic || s.underline || s.strike;

const HEX_DIGIT = /^[0-9a-f]$/i;

/** parseFormatting strips § and ANSI codes from a log line and returns the
 *  styled runs they described. */
export function parseFormatting(raw: string): Formatted {
  if (!raw.includes("§") && !raw.includes("\x1b")) return { text: raw };

  const segments: Segment[] = [];
  let style: SegmentStyle = {};
  let run = "";
  let text = "";

  const setStyle = (next: SegmentStyle) => {
    if (run) {
      segments.push({ ...style, text: run });
      run = "";
    }
    style = next;
  };

  let i = 0;
  while (i < raw.length) {
    const ch = raw[i];
    if (ch === "§") {
      const code = (raw[i + 1] ?? "").toLowerCase();
      if (code in MC_COLORS) {
        // A colour code also clears bold, italic and the rest, as in game.
        setStyle({ color: MC_COLORS[code] });
      } else if (code === "l") setStyle({ ...style, bold: true });
      else if (code === "o") setStyle({ ...style, italic: true });
      else if (code === "n") setStyle({ ...style, underline: true });
      else if (code === "m") setStyle({ ...style, strike: true });
      else if (code === "r") setStyle({});
      else if (code === "x") {
        // §x§R§R§G§G§B§B: six § pairs, one hex digit each.
        let hex = "";
        for (let k = 0; k < 6; k++) {
          const at = i + 2 + k * 2;
          if (raw[at] !== "§" || !HEX_DIGIT.test(raw[at + 1] ?? "")) break;
          hex += raw[at + 1];
        }
        if (hex.length === 6) {
          setStyle({ color: `#${hex.toLowerCase()}` });
          i += 14;
          continue;
        }
      }
      // §k (scrambled text) and unknown codes only disappear. A § at the very
      // end of the line has no code to take with it.
      i += code ? 2 : 1;
      continue;
    }
    if (ch === "\x1b") {
      // CSI: ESC [ parameters final-byte. Only SGR (final "m") styles text; the
      // rest (cursor moves, line clears) are dropped.
      const m = /^\x1b\[([0-9;?]*)([@-~])/.exec(raw.slice(i, i + 64));
      if (m) {
        if (m[2] === "m") setStyle(applySgr(style, m[1]));
        i += m[0].length;
      } else {
        i += 1;
      }
      continue;
    }
    run += ch;
    text += ch;
    i++;
  }
  setStyle({});

  return segments.some(styled) ? { text, segments } : { text };
}
