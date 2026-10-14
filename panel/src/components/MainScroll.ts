import { createContext, useCallback, useContext } from "react";

/** MainScrollContext is the shell's scrolling content area. Pages scroll inside it rather
 *  than the window, so moving to another page or section brings it back to the top itself. */
export const MainScrollContext = createContext<HTMLElement | null>(null);

/** useScrollToTop returns a function that brings the content area back to its top. */
export function useScrollToTop(): () => void {
  const main = useContext(MainScrollContext);
  return useCallback(() => {
    if (main) main.scrollTop = 0;
  }, [main]);
}
