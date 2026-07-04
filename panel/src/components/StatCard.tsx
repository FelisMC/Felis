import type { LucideIcon } from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { cn } from "@/lib/utils";

export interface StatCardProps {
  icon: LucideIcon;
  label: string;
  value: React.ReactNode;
  /** Tailwind classes applied to the icon container (e.g. "text-amber-500 bg-amber-500/10") */
  accentClass?: string;
  /** A hex colour string; applied as `color` on the icon container. Takes precedence over accentClass for the colour. */
  accentColor?: string;
  /** Tailwind classes applied to the value text */
  valueClass?: string;
}

const DEFAULT_ICON_CONTAINER = "rounded-md p-2 bg-muted/30 text-muted-foreground";

export function StatCard({ icon: Icon, label, value, accentClass, accentColor, valueClass }: StatCardProps) {
  const containerStyle = accentColor ? { backgroundColor: `${accentColor}26`, color: accentColor } : undefined;
  return (
    <Card>
      <CardContent className="flex items-center gap-3 p-4">
        <div
          className={cn(DEFAULT_ICON_CONTAINER, !accentColor && accentClass)}
          style={containerStyle}
        >
          <Icon className="h-5 w-5" />
        </div>
        <div className="min-w-0 flex-1">
          <div
            className={cn(
              "text-2xl font-bold font-mono leading-none truncate text-foreground",
              valueClass,
            )}
            title={typeof value === "string" ? value : undefined}
          >
            {value}
          </div>
          <div className="mt-1 text-[11px] text-muted-foreground font-medium">{label}</div>
        </div>
      </CardContent>
    </Card>
  );
}
