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

// The shell navigation model, kept as plain data so the "which sections does this
// identity see" decision is a pure function (visibleSections) that vitest can pin
// without rendering React. The three sections map onto DESIGN-WEB-3SIDES §2:
// User-Side is always present (app-tier); Admin-Side and SysAdmin-Side are a
// navigational separation of *concern* over the SAME admin tier — both gated by
// the one `is_admin` flag, surfaced as two sections only for admins.
//
// Hiding a section is UX convenience, NOT a security control: every /admin and
// /ops data call is independently 403-gated server-side (the RequireAdmin route
// wrapper is belt-and-suspenders on top of that).

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
      { to: "/admin/updates", key: "admin_updates", icon: Clock },
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
      { to: "/admin/platform", key: "platform_settings", icon: Settings },
    ],
  },
];

/**
 * visibleSections returns the sections a caller with the given admin flag may see.
 * Pure and total: a non-admin (or the fail-closed `false` used while /me is still
 * loading or after it errors) gets exactly the User-Side section; an admin gets all
 * admin sections; an owner additionally gets the owner-gated sections. This is the
 * single decision the sidebar renders from.
 */
export function visibleSections(isAdmin: boolean, isOwner: boolean): NavSection[] {
  return NAV_SECTIONS.filter((s) => {
    if (s.ownerOnly) return isOwner;
    if (s.adminOnly) return isAdmin;
    return true;
  });
}
