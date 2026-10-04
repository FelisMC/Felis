import logo from "@/assets/felis-logo.svg";

export function FelisLogo({ size = 28 }: { size?: number }) {
  return <img src={logo} alt="" aria-hidden="true" width={size} height={size} className="shrink-0" />;
}
