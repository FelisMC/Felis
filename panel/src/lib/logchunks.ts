import type { LogLine } from "./logstream";

// Stable sequence chunks limit rerenders while retaining all DOM lines for browser search and copying.
export const CHUNK_LINES = 100;

/** chunkLines groups a buffer into chunks keyed by seq / CHUNK_LINES. */
export function chunkLines(lines: LogLine[]): { key: number; lines: LogLine[] }[] {
  const chunks: { key: number; lines: LogLine[] }[] = [];
  for (const line of lines) {
    const key = Math.floor(line.seq / CHUNK_LINES);
    const last = chunks[chunks.length - 1];
    if (last?.key === key) last.lines.push(line);
    else chunks.push({ key, lines: [line] });
  }
  return chunks;
}

// Immutable consecutive sequences make matching first and last lines sufficient for chunk equality.
export function sameChunk(a: { lines: LogLine[] }, b: { lines: LogLine[] }): boolean {
  return a.lines[0] === b.lines[0] && a.lines[a.lines.length - 1] === b.lines[b.lines.length - 1];
}
