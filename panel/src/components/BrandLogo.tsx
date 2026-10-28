import { FelisLogo } from "@/components/FelisLogo";
import { useBranding } from "@/lib/branding";

/** BrandLogo shows the logo the Owner uploaded, or the Felis mark when there is none. */
export function BrandLogo({ size = 28 }: { size?: number }) {
  const { name, logo } = useBranding();
  if (!logo) return <FelisLogo size={size} />;
  return <img src={logo} alt={name} width={size} height={size} className="shrink-0 object-contain" />;
}
