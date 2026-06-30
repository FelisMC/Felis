import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { useTier } from "./tier";
import {
  availableViewModes,
  effectiveViewMode,
  parseViewMode,
  type ViewMode,
} from "./viewmode";

// The React layer over viewmode.ts: it holds the principal's *requested* home and
// hands consumers the *resolved* one, re-gated against the live is_admin flag on
// every render. It mirrors theme.tsx (createContext + Provider + useX hook) but the
// resolution rule is deliberately split into a raw seed and a live gate, because the
// two answer different questions:
//
//   * `selected` is the raw intent — whatever home the principal last chose, parsed
//     for shape only (parseViewMode), NOT gated. It is seeded from localStorage and
//     updated on each switch. It may legitimately hold "ops" even while isAdmin is
//     momentarily false (still loading /me, or a transient failure).
//   * `view` is the resolved home — effectiveViewMode(selected, isAdmin) recomputed
//     every render. THIS is the only value any consumer is allowed to act on, and it
//     fails closed: a non-admin, or an admin mid-demotion, always sees "user".
//
// Why split rather than call restoreViewMode once at seed time: restoreViewMode gates
// at the moment it runs, and at boot isAdmin is fail-closed `false` while /me is in
// flight. Gating then would discard an admin's stored "ops" before identity arrives.
// Keeping the raw intent in `selected` and gating live means the admin's chosen home
// is restored the instant isAdmin flips true — and an unentitled value is held below
// "user" the whole time. The persisted value is therefore never trusted on its own;
// it only ever survives as raw intent and is re-gated on every read.

const VIEW_STORAGE_KEY = "felis.viewmode";

type ViewModeState = {
  /** The resolved, fail-closed home the principal is actually in. Act on this only. */
  view: ViewMode;
  /** The homes this principal may switch into, given the live admin flag. */
  available: ViewMode[];
  /** Request a switch. The next home is re-gated before it is applied or persisted,
   *  so an unentitled value can never be stored or shown. */
  setView: (next: ViewMode) => void;
};

const ViewModeContext = createContext<ViewModeState>({
  view: "user",
  available: ["user"],
  setView: () => {},
});

// ---- persistence (SSR- and private-mode-safe, like theme.tsx's matchMedia guards) ----

function readStored(): unknown {
  if (typeof window === "undefined" || !window.localStorage) return null;
  try {
    return window.localStorage.getItem(VIEW_STORAGE_KEY);
  } catch {
    // Storage can throw in private mode or when disabled by policy; treat as absent.
    return null;
  }
}

function writeStored(view: ViewMode) {
  if (typeof window === "undefined" || !window.localStorage) return;
  try {
    window.localStorage.setItem(VIEW_STORAGE_KEY, view);
  } catch {
    // Best-effort: a failed persist just means the choice won't survive a reload.
  }
}

// ---- provider ----

export function ViewModeProvider({ children }: { children: ReactNode }) {
  const { isAdmin } = useTier();

  // Seed with the RAW stored intent (shape-checked, not gated) so an admin's stored
  // "ops" survives the /me loading window and is restored once isAdmin resolves true.
  const [selected, setSelected] = useState<ViewMode | null>(() =>
    parseViewMode(readStored()),
  );

  // The live gate: resolved every render, so a demotion or transient /me failure
  // (isAdmin → false) collapses the home to "user" immediately, with no flash.
  const view = effectiveViewMode(selected, isAdmin);
  const available = useMemo(() => availableViewModes(isAdmin), [isAdmin]);

  const setView = useCallback(
    (next: ViewMode) => {
      // Re-gate at the source: only a home the principal is entitled to right now is
      // stored or applied, so the app's own writes can never persist an escalation.
      const resolved = effectiveViewMode(next, isAdmin);
      setSelected(resolved);
      writeStored(resolved);
    },
    [isAdmin],
  );

  const value = useMemo<ViewModeState>(
    () => ({ view, available, setView }),
    [view, available, setView],
  );

  return (
    <ViewModeContext.Provider value={value}>{children}</ViewModeContext.Provider>
  );
}

export function useViewMode() {
  return useContext(ViewModeContext);
}
