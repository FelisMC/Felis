import { createContext, useCallback, useContext } from "react";

// Pages scroll inside this shell container rather than the window.
export const MainScrollContext = createContext<HTMLElement | null>(null);

/** useScrollToTop returns a function that brings the content area back to its top. */
export function useScrollToTop(): () => void {
  const main = useContext(MainScrollContext);
  return useCallback(() => {
    if (main) main.scrollTop = 0;
  }, [main]);
}
