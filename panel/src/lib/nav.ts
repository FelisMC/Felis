import {
  DoorOpen,
  LayoutDashboard,
  Server,
  UserRound,
  Users,
  Boxes,
  Cpu,
  ClipboardCheck,
  Upload,
  Clock,
  ShieldCheck,
  Settings,
  type LucideIcon,
} from "lucide-react";

// Navigation visibility is presentation only; every privileged API route independently enforces access.

export interface NavItem {
  to: string;
  key: string;
  icon: LucideIcon;
  /** react-router NavLink `end` — exact-match active styling for index routes. */
  end?: boolean;
}

export interface NavSection {
  id: "user" | "admin" | "owner";
  /** Section heading key; null renders no heading (User-Side flat list). */
  titleKey: string | null;
  /** When true the section is shown only to admins (is_admin === true). */
  adminOnly: boolean;
  /** When true the section is shown only to the owner (is_owner === true).
   *  An owner-visible section is implicitly invisible to a plain admin. */
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
      { to: "/submissions", key: "my_submissions", icon: Upload },
      { to: "/account", key: "account", icon: UserRound },
    ],
  },
  {
    id: "admin",
    titleKey: "admin_section",
    adminOnly: true,
    ownerOnly: false,
    items: [
      { to: "/admin/lobby", key: "admin_lobby", icon: DoorOpen },
      { to: "/admin/images", key: "admin_images", icon: Boxes },
      { to: "/admin/builds", key: "admin_builds", icon: Cpu },
      { to: "/admin/submissions", key: "admin_submissions", icon: ClipboardCheck },
    ],
  },
  {
    id: "owner",
    titleKey: "owner_section",
    adminOnly: false,
    ownerOnly: true,
    items: [
      { to: "/admin/users", key: "admin_users", icon: Users },
      { to: "/admin/auth-sources", key: "auth_sources", icon: ShieldCheck },
      { to: "/admin/updates", key: "admin_updates", icon: Clock },
      { to: "/admin/platform", key: "platform_settings", icon: Settings },
    ],
  },
];

// Unknown or loading identity gets user sections only.
export function visibleSections(isAdmin: boolean, isOwner: boolean): NavSection[] {
  return NAV_SECTIONS.filter((s) => {
    if (s.ownerOnly) return isOwner;
    if (s.adminOnly) return isAdmin;
    return true;
  });
}
