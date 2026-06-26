import {
  createContext,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from "react";
import type { Identity } from "./types";
import { api } from "./api";

// TierProvider fetches GET /me exactly once at boot and exposes the result through
// context. Two design rules, both load-bearing:
//
//  1. Fail-closed: `isAdmin` is `identity?.is_admin === true`. While /me is in
//     flight (`identity === null`) or after it rejects, isAdmin is false — admin
//     nav/routes stay hidden. The `=== true` (not just truthy) also closes the
//     wire-shape trap: if the JSON ever arrives without `is_admin`, the value is
//     `undefined`, which is correctly non-admin rather than a thrown access.
//
//  2. Still-functional on failure: a /me rejection is caught and swallowed to a
//     null identity, NOT re-thrown. A logged-in user whose /me momentarily fails
//     still gets the full User-Side app; they simply don't see admin surfaces.
//     (The backend 403s admin data calls independently, so this is safe.)
//
// This is UX truth, not a security control — see DESIGN-WEB-3SIDES §1.

export interface TierState {
  /** The caller's identity, or null while loading or after a failed /me. */
  identity: Identity | null;
  /** True only while the initial /me request is in flight. */
  loading: boolean;
  /** Server-computed admin flag; false while loading or on failure (fail-closed). */
  isAdmin: boolean;
}

const TierContext = createContext<TierState>({
  identity: null,
  loading: true,
  isAdmin: false,
});

export function TierProvider({ children }: { children: ReactNode }) {
  const [identity, setIdentity] = useState<Identity | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let alive = true;
    api
      .me()
      .then((id) => {
        if (alive) setIdentity(id);
      })
      .catch(() => {
        // Fail-closed but functional: stay non-admin, keep rendering User-Side.
        if (alive) setIdentity(null);
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, []);

  const isAdmin = identity?.is_admin === true;

  return (
    <TierContext.Provider value={{ identity, loading, isAdmin }}>
      {children}
    </TierContext.Provider>
  );
}

/** useTier reads the boot-time identity/tier state. */
export function useTier(): TierState {
  return useContext(TierContext);
}
