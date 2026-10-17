import { api } from "@/lib/api";
import { opErrorText } from "./opText";
import { forgetSession, sendInParts, watchOp } from "./sessionUpload";
import { ONE_REQUEST_BYTES } from "./useUploads";

/** uploadFile lands one file at path in a server's world volume, the way the
 *  files page sends it: in one request up to ONE_REQUEST_BYTES, in parts above,
 *  following the op that lands the parts to its end. The folders on the way are
 *  made first, since an upload into a missing folder is refused. It settles once
 *  the file is on the volume; a refusal or a failed op rejects with an error
 *  humanizeError can word. */
export async function uploadFile(
  server: string,
  path: string,
  file: File,
  opts: { onProgress?: (sent: number) => void; signal?: AbortSignal } = {},
): Promise<void> {
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
    await api.uploadServerFile(server, path, file, false, opts);
    return;
  }
  const op = await sendInParts(server, path, file, { overwrite: false, ...opts });
  const end = op.state === "running" ? await watchOp(server, op.id, { signal: opts.signal }) : op;
  if (end.state === "failed") {
    const e = end.error ?? { code: "job_failed", message: "" };
    throw { status: 0, code: e.code, message: opErrorText("upload", e) };
  }
  forgetSession(server, path);
}
