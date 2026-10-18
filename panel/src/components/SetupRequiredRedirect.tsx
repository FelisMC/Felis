import { useEffect } from "react";
import { useNavigate } from "react-router-dom";
import { SETUP_REQUIRED_EVENT } from "@/lib/api";

// Keep the onboarding redirect inside Router but outside RequireAuth, matching /setup.
export function SetupRequiredRedirect() {
  const navigate = useNavigate();
  useEffect(() => {
    const toSetup = () => navigate("/setup", { replace: true });
    window.addEventListener(SETUP_REQUIRED_EVENT, toSetup);
    return () => window.removeEventListener(SETUP_REQUIRED_EVENT, toSetup);
  }, [navigate]);
  return null;
}
