import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";

export type Theme = "light" | "dark";

type ThemeProviderState = {
  /** The resolved, active theme ("light" | "dark") applied to the DOM. */
  theme: Theme;
  setTheme: (theme: Theme) => void;
  toggleTheme: () => void;
};

const ThemeContext = createContext<ThemeProviderState>({
  theme: "dark",
  setTheme: () => {},
  toggleTheme: () => {},
});

const SYSTEM_DARK_QUERY = "(prefers-color-scheme: dark)";

// ---- system theme store ----

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

// ---- provider ----

export function ThemeProvider({ children }: { children: React.ReactNode }) {
  const [theme, setThemeState] = useState<Theme>(systemThemeSnapshot);

  const setTheme = useCallback(
    (next: Theme) => {
      // Direct DOM write to prevent visual lag on transition.
      setThemeState(next);
      applyClass(next);
    },
    [],
  );

  const toggleTheme = useCallback(() => {
    setTheme(theme === "dark" ? "light" : "dark");
  }, [theme, setTheme]);

  useEffect(() => {
    const mql = systemThemeQuery();
    if (!mql) return;

    const syncSystemTheme = () => setThemeState(mql.matches ? "dark" : "light");
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
    () => ({ theme, setTheme, toggleTheme }),
    [theme, setTheme, toggleTheme],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

export function useTheme() {
  return useContext(ThemeContext);
}
