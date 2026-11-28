import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { api } from "./api";
import { useConfig } from "./hooks";
import { useTier } from "./tier";
import type { PlatformFeaturesSettings } from "./types";

export const CLOSED_FEATURES: PlatformFeaturesSettings = {
  mode: "team",
  useLobby: false,
  lobbyAvailable: false,
  customImages: false,
  allowSubmissions: false,
  requireAccountLink: false,
  revision: "",
};

const STANDALONE_FEATURES: PlatformFeaturesSettings = {
  mode: "community",
  useLobby: true,
  lobbyAvailable: true,
  customImages: true,
  allowSubmissions: true,
  requireAccountLink: true,
  revision: "",
};

export interface FeaturesState {
  features: PlatformFeaturesSettings;
  loaded: boolean;
  apply: (next: PlatformFeaturesSettings) => void;
  reload: () => Promise<void>;
}

export const FeaturesContext = createContext<FeaturesState>({
  features: STANDALONE_FEATURES,
  loaded: true,
  apply: () => {},
  reload: async () => {},
});

export function FeaturesProvider({ children }: { children: ReactNode }) {
  const { identity } = useTier();
  const config = useConfig();
  const [features, setFeatures] = useState<PlatformFeaturesSettings>(CLOSED_FEATURES);
  const [loaded, setLoaded] = useState(false);
  const seq = useRef(0);

  const reload = useCallback(async () => {
    const ticket = ++seq.current;
    if (!identity || config?.nano) {
      setFeatures(CLOSED_FEATURES);
      setLoaded(false);
      return;
    }
    try {
      const next = await api.getFeatures();
      if (ticket !== seq.current) return;
      setFeatures(next);
      setLoaded(true);
    } catch {
      if (ticket !== seq.current) return;
      setLoaded(false);
    }
  }, [identity, config?.nano]);

  useEffect(() => {
    void reload();
  }, [reload]);

  const apply = useCallback((next: PlatformFeaturesSettings) => {
    seq.current += 1;
    setFeatures(next);
    setLoaded(true);
  }, []);

  return (
    <FeaturesContext.Provider value={{ features, loaded, apply, reload }}>
      {children}
    </FeaturesContext.Provider>
  );
}

export function useFeatures(): FeaturesState {
  return useContext(FeaturesContext);
}
