import {
  DoorOpen,
  LayoutDashboard,
  Server,
  UserRound,
  Users,
  Boxes,
  Upload,
  Clock,
  ShieldCheck,
  Settings,
  ScrollText,
  type LucideIcon,
} from "lucide-react";

// Navigation visibility is presentation only; every privileged API route independently enforces access.

export type NavFeatureGate = "customImages" | "submissions";

export interface NavItem {
  to: string;
  key: string;
  icon: LucideIcon;
  end?: boolean;
  noLobbyKey?: string;
  nano?: boolean;
  requires?: NavFeatureGate;
}

export interface InstallKind {
  noLobby?: boolean;
  nano?: boolean;
  customImages?: boolean;
  allowSubmissions?: boolean;
}

export interface NavSection {
  id: "user" | "admin" | "owner";
  titleKey: string | null;
  adminOnly: boolean;
  ownerOnly: boolean;
  items: NavItem[];
}

export const NAV_SECTIONS: NavSection[] = [
  {
    id: "user",
    titleKey: null,
    adminOnly: false,
    ownerOnly: false,
    items: [
      { to: "/", key: "dashboard", icon: LayoutDashboard, end: true },
      { to: "/servers", key: "my_servers", icon: Server },
      { to: "/submissions", key: "my_submissions", icon: Upload, requires: "submissions" },
      { to: "/account", key: "account", icon: UserRound, nano: true },
    ],
  },
  {
    id: "admin",
    titleKey: "admin_section",
    adminOnly: true,
    ownerOnly: false,
    items: [
      { to: "/admin/lobby", key: "admin_lobby", noLobbyKey: "admin_login_space", icon: DoorOpen },
      { to: "/admin/images", key: "admin_images_hub", icon: Boxes, requires: "customImages" },
    ],
  },
  {
    id: "owner",
    titleKey: "owner_section",
    adminOnly: false,
    ownerOnly: true,
    items: [
      { to: "/admin/users", key: "admin_users", icon: Users, nano: true },
      { to: "/admin/auth-sources", key: "auth_sources", icon: ShieldCheck, nano: true },
      { to: "/admin/updates", key: "admin_updates", icon: Clock },
      { to: "/admin/audit", key: "admin_audit", icon: ScrollText, nano: true },
      { to: "/admin/platform", key: "platform_settings", icon: Settings, nano: true },
    ],
  },
];

function navItemVisible(item: NavItem, install: InstallKind): boolean {
  if (install.nano && !item.nano) return false;
  if (item.requires === "customImages") return install.customImages === true;
  if (item.requires === "submissions") return install.customImages === true && install.allowSubmissions === true;
  return true;
}

// Unknown or loading identity gets user sections only.
export function visibleSections(isAdmin: boolean, isOwner: boolean, install: InstallKind = {}): NavSection[] {
  return NAV_SECTIONS.filter((s) => {
    if (s.ownerOnly) return isOwner;
    if (s.adminOnly) return isAdmin;
    return true;
  })
    .map((s) => ({
      ...s,
      items: s.items
        .filter((item) => navItemVisible(item, install))
        .map((item) => (install.noLobby && item.noLobbyKey ? { ...item, key: item.noLobbyKey } : item)),
    }))
    .filter((s) => s.items.length > 0);
}
