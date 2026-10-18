import type { ApiError } from "./types";

// Return hex and RFC 9530 digests; unreadable files fail before upload.
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

// Hash decoded base64 file bytes for read/write integrity checks; invalid base64 throws.
export async function base64Sha256(b64: string): Promise<string> {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return (await sha256Of(new Blob([bytes]))).hex;
}
