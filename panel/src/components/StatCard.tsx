import type { LucideIcon } from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { cn } from "@/lib/utils";

export interface StatCardProps {
  icon: LucideIcon;
  label: string;
  value: React.ReactNode;
  accentClass?: string;
  accentColor?: string;
  valueClass?: string;
}

export function StatCard({ icon: Icon, label, value, accentColor, valueClass }: StatCardProps) {
  return (
    <Card>
      <CardContent className="flex items-center justify-between gap-3 px-3.5 py-3">
        <div className="min-w-0 flex-1">
          <div
            className={cn(
              "truncate font-mono text-lg font-semibold leading-tight tracking-tight text-foreground tabular-nums sm:text-xl",
              valueClass,
            )}
            title={typeof value === "string" ? value : undefined}
          >
            {value}
          </div>
          <div className="mt-1 truncate text-[11px] font-medium text-muted-foreground">{label}</div>
        </div>
        <div
          className="shrink-0 rounded-md border border-border/70 bg-muted p-1.5 text-muted-foreground"
          style={accentColor ? { color: accentColor } : undefined}
          aria-hidden="true"
        >
          <Icon className="h-4 w-4" />
        </div>
      </CardContent>
    </Card>
  );
}
