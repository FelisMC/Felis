import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "@/lib/api";
import { useAsync } from "@/lib/hooks";
import { touchesWorld, worldDirectories } from "./worldPaths";

export type FileWorldConfirmation = ReturnType<typeof useFileWorldConfirmation>;

export function useFileWorldConfirmation(server: string, owned: boolean, live: boolean) {
  const { t } = useTranslation("files");
  const [confirmed, setConfirmedState] = useState(false);
  // Handlers read the ref so an action resumed right after confirming sees it.
  const confirmedRef = useRef(false);
  const setConfirmed = useCallback((value: boolean) => {
    confirmedRef.current = value;
    setConfirmedState(value);
  }, []);
  useEffect(() => setConfirmed(false), [server, live, setConfirmed]);
  const [pending, setPending] = useState<(() => void) | null>(null);
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

  function blocked(paths: readonly string[], ok: boolean): boolean {
    return live && !ok && (paths.length === 0 || !directories || paths.some((path) => touchesWorld(path, directories)));
  }

  function needsConfirmation(paths: readonly string[]): boolean {
    return blocked(paths, confirmed);
  }

  // A refused write opens the question, so a retry after confirming goes through.
  function requireConfirmation(paths: readonly string[]) {
    if (!blocked(paths, confirmedRef.current)) return;
    setPending(() => () => {});
    throw new Error(t("live_world_confirm_required"));
  }

  /** guard tells the caller to go ahead now, or asks first and calls run once confirmed. */
  function guard(paths: readonly string[], run: () => void): boolean {
    if (!blocked(paths, confirmedRef.current)) return true;
    setPending(() => run);
    return false;
  }

  function accept() {
    setConfirmed(true);
    const run = pending;
    setPending(null);
    run?.();
  }

  return {
    confirmed, setConfirmed, directories, needsConfirmation, requireConfirmation, guard,
    asking: pending !== null, accept, dismiss: () => setPending(null), reload: query.reload,
  };
}
