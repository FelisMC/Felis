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
import { ACCESS_REFUSED_EVENT, api, SESSION_EXPIRED_EVENT } from "./api";
import { deriveAuth, isUnauthorized, type AuthState } from "./auth";

// Only 401 signs out; missing or malformed identity flags grant no elevated role.
// Revalidate on protected-route errors, focus, and visibility changes without unmounting the app.

export interface TierState extends AuthState {
  /** Owner (platform-level, one above admin) — exposed so nav can show the Users
   *  section only to the owner identity. Computed server-side, fail-closed. */
  isOwner: boolean;
  /** Re-fetch /me and recompute the auth state. Awaitable so callers can sequence a
   *  navigation after the context has settled (login → refresh → redirect). */
  refresh: () => Promise<void>;
  /** Re-read /me in the background, keeping the app mounted. A 401 signs the
   *  person out (with sessionEnded set); any other failure changes nothing. */
  revalidate: () => Promise<void>;
  /** True once a signed-in session was found to have ended (expired or revoked). */
  sessionEnded: boolean;
}

// A tab returning to view re-checks /me at most this often; a 401 from a real
// call re-checks at once.
const REVALIDATE_EVERY_MS = 60_000;
// A role refusal from a real call re-checks at most this often, so a page that
// keeps polling a refused route does not re-read /me on every tick.
export const ACCESS_RECHECK_MS = 10_000;

const TierContext = createContext<TierState>({
  identity: null,
  loading: true,
  isAdmin: false,
  isOwner: false,
  unauthenticated: false,
  identityError: null,
  refresh: async () => {},
  revalidate: async () => {},
  sessionEnded: false,
});

export function TierProvider({ children }: { children: ReactNode }) {
  const [identity, setIdentity] = useState<Identity | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [loading, setLoading] = useState(true);

  // A monotonic ticket guards against out-of-order refreshes (e.g. a logout's /me
  // resolving after a subsequent login's): only the latest call commits its result.
  const seq = useRef(0);

  const [sessionEnded, setSessionEnded] = useState(false);
  const loadingRef = useRef(true);
  const identityRef = useRef<Identity | null>(null);
  const revalidating = useRef(false);
  const lastCheck = useRef(0);

  const refresh = useCallback(async () => {
    const ticket = ++seq.current;
    loadingRef.current = true;
    setLoading(true);
    try {
      const id = await api.me();
      if (ticket === seq.current) {
        identityRef.current = id;
        setIdentity(id);
        setError(null);
        setSessionEnded(false);
      }
    } catch (e) {
      // Keep the error so deriveAuth can tell a 401 (→ login) from a transient
      // failure (→ stay functional). Identity is cleared either way.
      if (ticket === seq.current) {
        identityRef.current = null;
        setIdentity(null);
        setError(e);
      }
    } finally {
      if (ticket === seq.current) {
        loadingRef.current = false;
        setLoading(false);
      }
      lastCheck.current = Date.now();
    }
  }, []);

  const revalidate = useCallback(async () => {
    // A refresh in flight answers the same question and owns the loading flag;
    // bumping the ticket under it would leave the spinner up for good.
    if (loadingRef.current || revalidating.current) return;
    revalidating.current = true;
    const ticket = ++seq.current;
    try {
      const id = await api.me();
      if (ticket === seq.current) {
        // An unchanged identity keeps its object so nothing re-renders.
        if (JSON.stringify(id) !== JSON.stringify(identityRef.current)) {
          identityRef.current = id;
          setIdentity(id);
        }
        setError(null);
      }
    } catch (e) {
      if (ticket === seq.current && isUnauthorized(e)) {
        if (identityRef.current !== null) setSessionEnded(true);
        identityRef.current = null;
        setIdentity(null);
        setError(e);
      }
    } finally {
      revalidating.current = false;
      lastCheck.current = Date.now();
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  useEffect(() => {
    const onExpired = () => void revalidate();
    const onReturn = () => {
      if (document.visibilityState !== "visible") return;
      if (Date.now() - lastCheck.current < REVALIDATE_EVERY_MS) return;
      void revalidate();
    };
    const onRefused = () => {
      if (Date.now() - lastCheck.current < ACCESS_RECHECK_MS) return;
      void revalidate();
    };
    window.addEventListener(SESSION_EXPIRED_EVENT, onExpired);
    window.addEventListener(ACCESS_REFUSED_EVENT, onRefused);
    window.addEventListener("focus", onReturn);
    document.addEventListener("visibilitychange", onReturn);
    return () => {
      window.removeEventListener(SESSION_EXPIRED_EVENT, onExpired);
      window.removeEventListener(ACCESS_REFUSED_EVENT, onRefused);
      window.removeEventListener("focus", onReturn);
      document.removeEventListener("visibilitychange", onReturn);
    };
  }, [revalidate]);

  const state = deriveAuth(identity, error, loading);
  // Owner and admin flags fail closed independently.
  const isOwner = identity?.is_owner === true;

  return (
    <TierContext.Provider value={{ ...state, isOwner, refresh, revalidate, sessionEnded }}>
      {children}
    </TierContext.Provider>
  );
}

/** useTier reads the current identity/tier/auth state. */
export function useTier(): TierState {
  return useContext(TierContext);
}
