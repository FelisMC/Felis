let offset = 0;

/** noteServerDate keeps how far the server's clock is from this browser's, from a response's Date header. */
export function noteServerDate(header: string | null): void {
  const at = header ? Date.parse(header) : NaN;
  if (!Number.isNaN(at)) offset = at - Date.now();
}

/** serverNow reads the server's clock, so timestamps the server wrote line up even when the two clocks differ. */
export function serverNow(): number {
  return Date.now() + offset;
}
