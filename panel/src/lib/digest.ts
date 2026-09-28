import type { ApiError } from "./types";

// sha256Of hashes the bytes of blob the way felis-api checks an upload body:
// `hex` is how the server lists the parts it holds, and `header` is the
// Content-Digest value (RFC 9530) a request carries it in. A file changed or
// removed on disk since it was picked cannot be read (the browser's
// NotReadableError), which rejects as file_unreadable before anything is sent.
export async function sha256Of(blob: Blob): Promise<{ hex: string; header: string }> {
  let bytes: ArrayBuffer;
  try {
    bytes = await blob.arrayBuffer();
  } catch (e) {
    const err: ApiError = { status: 0, code: "file_unreadable", message: e instanceof Error ? e.message : String(e) };
    throw err;
  }
  const sum = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  let bin = "";
  let hex = "";
  for (const b of sum) {
    bin += String.fromCharCode(b);
    hex += b.toString(16).padStart(2, "0");
  }
  return { hex, header: `sha-256=:${btoa(bin)}:` };
}
