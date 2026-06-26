import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import type { Identity } from "./types";
import { api } from "./api";
import { deriveAuth, type AuthState } from "./auth";

// TierProvider fetches GET /me at boot and re-fetches on demand (refresh), exposing
// the result through context. Three design rules, all load-bearing:
//
//  1. Fail-closed: `isAdmin` is `identity?.is_admin === true`. While /me is in
//     flight (`identity === null`) or after it rejects, isAdmin is false — admin
//     nav/routes stay hidden. The `=== true` (not just truthy) also closes the
//     wire-shape trap: if the JSON ever arrives without `is_admin`, the value is
//     `undefined`, which is correctly non-admin rather than a thrown access.
//
//  2. Still-functional on a transient failure: a /me rejection that is NOT a 401 is
//     swallowed to a null identity, NOT re-thrown and NOT treated as logged-out. A
//     principal whose /me momentarily fails still gets the full User-Side app; they
//     simply don't see admin surfaces. (The backend 403s admin data calls
//     independently, so this is safe.) Only a genuine 401 sets `unauthenticated`.
//
//  3. Login-aware: `unauthenticated` (a true 401) routes to /login;
//     `mustChangePassword` forces the change-password card; `refresh()` re-reads /me
//     after a login / change / logout so the gate re-evaluates without a reload.
//
// Rules 1–2 are UX truth, not a security control — see DESIGN-WEB-3SIDES §1.

export interface TierState extends AuthState {
  /** Re-fetch /me and recompute the auth state. Awaitable so callers can sequence a
   *  navigation after the context has settled (login → refresh → redirect). */
  refresh: () => Promise<void>;
}

const TierContext = createContext<TierState>({
  identity: null,
  loading: true,
  isAdmin: false,
  unauthenticated: false,
  mustChangePassword: false,
  refresh: async () => {},
});

export function TierProvider({ children }: { children: ReactNode }) {
  const [identity, setIdentity] = useState<Identity | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [loading, setLoading] = useState(true);

  // A monotonic ticket guards against out-of-order refreshes (e.g. a logout's /me
  // resolving after a subsequent login's): only the latest call commits its result.
  const seq = useRef(0);

  const refresh = useCallback(async () => {
    const ticket = ++seq.current;
    setLoading(true);
    try {
      const id = await api.me();
      if (ticket === seq.current) {
        setIdentity(id);
        setError(null);
      }
    } catch (e) {
      // Keep the error so deriveAuth can tell a 401 (→ login) from a transient
      // failure (→ stay functional). Identity is cleared either way.
      if (ticket === seq.current) {
        setIdentity(null);
        setError(e);
      }
    } finally {
      if (ticket === seq.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const state = deriveAuth(identity, error, loading);

  return (
    <TierContext.Provider value={{ ...state, refresh }}>
      {children}
    </TierContext.Provider>
  );
}

/** useTier reads the current identity/tier/auth state. */
export function useTier(): TierState {
  return useContext(TierContext);
}
