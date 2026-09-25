import { useEffect } from "react";
import { useNavigate } from "react-router-dom";
import { SETUP_REQUIRED_EVENT } from "@/lib/api";

// SetupRequiredRedirect listens for the `403 setup_required` signal api.ts emits
// when a session still owes forced onboarding (#8) and routes it to the wizard.
// It must live inside the Router (it navigates) and outside RequireAuth (/setup
// sits there too); the event fires from any protected call the app makes, so the
// listener is always mounted by the time one arrives.
export function SetupRequiredRedirect() {
  const navigate = useNavigate();
  useEffect(() => {
    const toSetup = () => navigate("/setup", { replace: true });
    window.addEventListener(SETUP_REQUIRED_EVENT, toSetup);
    return () => window.removeEventListener(SETUP_REQUIRED_EVENT, toSetup);
  }, [navigate]);
  return null;
}
