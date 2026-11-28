import { useLayoutEffect, useRef, useState, type RefObject } from "react";

export interface IndicatorRect {
  start: number;
  size: number;
  ready: boolean;
  animate: boolean;
}

const INITIAL: IndicatorRect = { start: 0, size: 0, ready: false, animate: false };

export function useSlidingIndicator(
  containerRef: RefObject<HTMLElement | null>,
  activeSelector: string,
  activeKey: string | number | null,
  axis: "x" | "y" = "x",
): IndicatorRect {
  const [rect, setRect] = useState<IndicatorRect>(INITIAL);
  const measuredRef = useRef(false);

  useLayoutEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    const measure = (animate: boolean) => {
      const active = container.querySelector<HTMLElement>(activeSelector);
      if (!active) {
        measuredRef.current = false;
        setRect((prev) => (prev.ready ? INITIAL : prev));
        return;
      }
      const cRect = container.getBoundingClientRect();
      const aRect = active.getBoundingClientRect();
      const size = axis === "x" ? aRect.width : aRect.height;
      if (size <= 0) return;
      const start =
        axis === "x"
          ? aRect.left - cRect.left - container.clientLeft + container.scrollLeft
          : aRect.top - cRect.top - container.clientTop + container.scrollTop;
      const shouldAnimate = animate && measuredRef.current;
      measuredRef.current = true;
      setRect((prev) =>
        prev.ready &&
        Math.abs(prev.start - start) < 0.5 &&
        Math.abs(prev.size - size) < 0.5
          ? prev
          : { start, size, ready: true, animate: shouldAnimate },
      );
    };

    measure(true);
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(() => measure(false));
    ro.observe(container);
    return () => ro.disconnect();
  }, [containerRef, activeSelector, activeKey, axis]);

  return rect;
}

export function useEnterAnimation(
  ref: RefObject<HTMLElement | null>,
  triggerKey: string,
  offsetY = -6,
  duration = 200,
  fromOpacity = 0.4,
) {
  const mountedRef = useRef(false);

  useLayoutEffect(() => {
    const el = ref.current;
    if (!el || typeof el.animate !== "function") return;
    if (window.matchMedia?.("(prefers-reduced-motion: reduce)").matches) return;
    const first = !mountedRef.current;
    mountedRef.current = true;
    if (first && offsetY === 0) return;
    el.getAnimations().forEach((anim) => anim.cancel());
    el.animate(
      [
        { opacity: fromOpacity, transform: offsetY ? `translateY(${offsetY}px)` : "none" },
        { opacity: 1, transform: "translateY(0)" },
      ],
      { duration, easing: "cubic-bezier(0.22, 1, 0.36, 1)" },
    );
  }, [ref, triggerKey, offsetY, duration, fromOpacity]);
}
