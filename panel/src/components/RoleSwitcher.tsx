import { useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Eye } from "lucide-react";
import { useTier } from "@/lib/tier";
import { useViewMode } from "@/lib/viewmode-store";
import {
  availableViewModes,
  effectiveViewMode,
  landingPathForView,
  viewModeLabelKey,
  type ViewMode,
} from "@/lib/viewmode";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

// The avatar role-switcher (DESIGN-WEB-3SIDES): an admin can move between the three
// homes — User-Side, Admin-Side, SysAdmin-Side — viewing the app as each. The control
// is admin-only and renders nothing for everyone else: availableViewModes returns just
// ["user"] for a non-admin (and during the fail-closed loading window), so there is
// no home to switch into and the whole widget collapses. This is UX, not a gate — the
// sidebar narrowing it drives only declutters; every /admin and /ops call is 403-gated
// server-side regardless of the chosen view.
export function RoleSwitcher() {
  const { isAdmin } = useTier();
  const { view, setView } = useViewMode();
  const navigate = useNavigate();
  const { t } = useTranslation("navigation");

  const modes = availableViewModes(isAdmin);
  // Only an admin has more than one home; everyone else gets no switcher at all.
  if (modes.length <= 1) return null;

  function onChange(raw: string) {
    // The menu only offers entitled homes, but re-gate anyway: the value crosses a
    // string boundary and effectiveViewMode is the single authority on what is allowed.
    const next = effectiveViewMode(raw as ViewMode, isAdmin);
    setView(next);
    // Land on the chosen home's root so switching shows a meaningful page rather than
    // whatever route the principal happened to be on.
    navigate(landingPathForView(next));
  }

  return (
    <Select value={view} onValueChange={onChange}>
      <SelectTrigger
        className="h-8 gap-2 px-2.5 text-[13px]"
        aria-label={t("view_switch_label")}
        title={t("view_switch_label")}
      >
        <span className="flex min-w-0 items-center gap-2">
          <Eye className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
          <SelectValue />
        </span>
      </SelectTrigger>
      <SelectContent align="start">
        {modes.map((m) => (
          <SelectItem key={m} value={m}>
            {t(viewModeLabelKey(m))}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}
