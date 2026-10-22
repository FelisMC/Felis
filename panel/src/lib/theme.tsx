import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";

export type Theme = "light" | "dark";
export type ThemePreference = Theme | "system";

type ThemeProviderState = {
  /** The resolved, active theme ("light" | "dark") applied to the DOM. */
  theme: Theme;
  preference: ThemePreference;
  setTheme: (theme: ThemePreference) => void;
};

const ThemeContext = createContext<ThemeProviderState>({
  theme: "dark",
  preference: "system",
  setTheme: () => {},
});

const SYSTEM_DARK_QUERY = "(prefers-color-scheme: dark)";

function savedPreference(): ThemePreference {
  try {
    const saved = window.localStorage.getItem("felis-theme");
    return saved === "light" || saved === "dark" ? saved : "system";
  } catch {
    return "system";
  }
}

function systemThemeQuery() {
  if (typeof window === "undefined" || !window.matchMedia) return null;
  return window.matchMedia(SYSTEM_DARK_QUERY);
}

function systemThemeSnapshot(): Theme {
  return systemThemeQuery()?.matches ? "dark" : "light";
}

function applyClass(t: Theme) {
  const root = document.documentElement;
  if (t === "dark") {
    root.classList.add("dark");
    root.classList.remove("light");
  } else {
    root.classList.add("light");
    root.classList.remove("dark");
  }
}

export function ThemeProvider({ children }: { children: React.ReactNode }) {
  const [preference, setPreference] = useState<ThemePreference>(savedPreference);
  const [systemTheme, setSystemTheme] = useState<Theme>(systemThemeSnapshot);
  const theme = preference === "system" ? systemTheme : preference;

  const setTheme = useCallback(
    (next: ThemePreference) => {
      setPreference(next);
      applyClass(next === "system" ? systemThemeSnapshot() : next);
      try {
        window.localStorage.setItem("felis-theme", next);
      } catch {
        // Keep the choice for this tab when browser storage is unavailable.
      }
    },
    [],
  );

  useEffect(() => {
    const mql = systemThemeQuery();
    if (!mql) return;

    const syncSystemTheme = () => setSystemTheme(mql.matches ? "dark" : "light");
    syncSystemTheme();

    if (mql.addEventListener) {
      mql.addEventListener("change", syncSystemTheme);
      return () => mql.removeEventListener("change", syncSystemTheme);
    }

    mql.addListener?.(syncSystemTheme);
    return () => mql.removeListener?.(syncSystemTheme);
  }, []);

  // Sync the class whenever the resolved theme changes.
  useEffect(() => {
    applyClass(theme);
  }, [theme]);

  const value = useMemo<ThemeProviderState>(
    () => ({ theme, preference, setTheme }),
    [theme, preference, setTheme],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

export function useTheme() {
  return useContext(ThemeContext);
}
