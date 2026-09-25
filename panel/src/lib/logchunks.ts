import type { LogLine } from "./logstream";

// Lines render in chunks of CHUNK_LINES, grouped by seq so a line never moves
// between chunks. A full chunk never changes again, so on each frame React
// re-renders only the tail chunk (and the head one while the ring buffer
// trims), whatever the buffer holds. Chunks off screen skip layout and paint
// through content-visibility. The DOM keeps every line, unlike a virtualised
// list, so find-in-page and selecting across lines to copy both keep working.
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

/** sameChunk: lines are immutable and a chunk's seqs are consecutive, so the
 *  same first and last line mean the same content. The first changes when the
 *  ring buffer trims the chunk, the last when a line lands in it. */
export function sameChunk(a: { lines: LogLine[] }, b: { lines: LogLine[] }): boolean {
  return a.lines[0] === b.lines[0] && a.lines[a.lines.length - 1] === b.lines[b.lines.length - 1];
}
