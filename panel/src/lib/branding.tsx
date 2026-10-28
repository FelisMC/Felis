import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { api } from "@/lib/api";
import type { PublicBranding } from "@/lib/types";

export const DEFAULT_BRANDING: PublicBranding = { name: "Felis", notice: "", show_powered_by: true, logo: null, background: null };

type BrandingState = {
  branding: PublicBranding;
  /** Rereads the branding after the settings change. */
  refresh: () => void;
};

const BrandingContext = createContext<BrandingState>({ branding: DEFAULT_BRANDING, refresh: () => {} });

export function BrandingProvider({ children }: { children: ReactNode }) {
  const [branding, setBranding] = useState(DEFAULT_BRANDING);
  const [reads, setReads] = useState(0);
  const refresh = useCallback(() => setReads((n) => n + 1), []);

  useEffect(() => {
    let alive = true;
    api.getBranding().then(
      (next) => alive && setBranding(next),
      () => {
        /* the current branding stays when it cannot be read */
      },
    );
    return () => {
      alive = false;
    };
  }, [reads]);

  const value = useMemo<BrandingState>(() => ({ branding, refresh }), [branding, refresh]);
  return <BrandingContext.Provider value={value}>{children}</BrandingContext.Provider>;
}

export function useBranding(): PublicBranding {
  return useContext(BrandingContext).branding;
}

export function useRefreshBranding(): () => void {
  return useContext(BrandingContext).refresh;
}
