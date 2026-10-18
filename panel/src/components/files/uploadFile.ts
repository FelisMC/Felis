import { api } from "@/lib/api";
import { opErrorText } from "./opText";
import { forgetSession, sendInParts, watchOp } from "./sessionUpload";
import { ONE_REQUEST_BYTES } from "./useUploads";

// Use one request below ONE_REQUEST_BYTES and resumable parts above it.
export async function uploadFile(
  server: string,
  path: string,
  file: File,
  opts: { onProgress?: (sent: number) => void; signal?: AbortSignal; overwrite?: boolean } = {},
): Promise<void> {
  const { overwrite = false, ...send } = opts;
  const dirs = path.split("/").slice(0, -1);
  for (let i = 1; i <= dirs.length; i++) {
    try {
      await api.mkdirServerFolder(server, dirs.slice(0, i).join("/"));
    } catch (e) {
      // A folder already there is the folder wanted.
      if ((e as { code?: string }).code !== "file_exists") throw e;
    }
  }
  if (file.size <= ONE_REQUEST_BYTES) {
    await api.uploadServerFile(server, path, file, overwrite, send);
    return;
  }
  const op = await sendInParts(server, path, file, { overwrite, ...send });
  const end = op.state === "running" ? await watchOp(server, op.id, { signal: send.signal }) : op;
  if (end.state === "failed") {
    const e = end.error ?? { code: "job_failed", message: "" };
    throw { status: 0, code: e.code, message: opErrorText("upload", e) };
  }
  forgetSession(server, path);
}
