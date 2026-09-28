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

// base64Sha256 answers the hex SHA-256 of the bytes b64 encodes: the file
// editor's content_sha256, which a save sends with its content and a read is
// checked against. A string that is not base64 throws.
export async function base64Sha256(b64: string): Promise<string> {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return (await sha256Of(new Blob([bytes]))).hex;
}
