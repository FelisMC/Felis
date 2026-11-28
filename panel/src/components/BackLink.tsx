import { ArrowLeft } from "lucide-react";
import { Link } from "react-router-dom";

interface Props {
  to: string;
  label: string;
}

export function BackLink({ to, label }: Props) {
  return (
    <Link
      to={to}
      className="group inline-flex h-7 items-center gap-1.5 rounded-md border border-border bg-card px-2.5 text-xs font-medium text-foreground transition-[color,background-color,border-color,transform] duration-150 ease-out hover:border-muted-foreground/30 hover:bg-accent/70 active:scale-[0.98]"
    >
      <ArrowLeft className="h-3.5 w-3.5 transition-transform duration-150 ease-out group-hover:-translate-x-0.5" /> {label}
    </Link>
  );
}
