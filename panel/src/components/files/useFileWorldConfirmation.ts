import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { touchesWorld, worldDirectories } from "./worldPaths";

export function useFileWorldConfirmation(server: string, owned: boolean, live: boolean) {
  const { t } = useTranslation("files");
  const [confirmed, setConfirmed] = useState(false);
  useEffect(() => setConfirmed(false), [server, live]);
  // eslint-disable-next-line react-hooks/exhaustive-deps -- Retain observed directory names until the next server run.
  const known = useMemo(() => new Set<string>(), [server, live]);
  const query = useAsync(async () => {
    if (!owned || !live) return null;
    const file = await api.readServerFile(server, "server.properties");
    const bytes = Uint8Array.from(atob(file.content), (char) => char.charCodeAt(0));
    const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    if (text.includes("\u0000")) return null;
    for (const dir of worldDirectories(text)) known.add(dir);
    return [...known];
  }, [server, owned, live, known]);
  const directories = query.loading || query.error ? null : query.data;

  function needsConfirmation(paths: readonly string[]): boolean {
    return live && !confirmed && (paths.length === 0 || !directories || paths.some((path) => touchesWorld(path, directories)));
  }

  function requireConfirmation(paths: readonly string[]) {
    if (needsConfirmation(paths)) throw new Error(t("live_world_confirm_required"));
  }

  return { confirmed, setConfirmed, directories, needsConfirmation, requireConfirmation, reload: query.reload };
}
