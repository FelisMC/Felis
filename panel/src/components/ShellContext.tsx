import React from "react";
import type { Phase } from "@/lib/types";

export interface ActiveServerMeta {
  name: string;
  displayName?: string;
  phase?: Phase;
}

export interface ShellContextValue {
  sidebarSlot: HTMLDivElement | null;
  activeServer: ActiveServerMeta | null;
  setActiveServer: (server: ActiveServerMeta | null) => void;
}

export const ShellContext = React.createContext<ShellContextValue | null>(null);
