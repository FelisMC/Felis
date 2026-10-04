import logo from "@/assets/felis-logo.svg";
import { cn } from "@/lib/utils";

export function FelisLogo({ className }: { className?: string }) {
  return <img src={logo} alt="" aria-hidden="true" width={1040} height={640} className={cn("shrink-0 object-contain", className)} />;
}
