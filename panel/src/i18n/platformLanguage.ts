import type { i18n } from "i18next";
import { api } from "@/lib/api";
import { chosenLanguage } from "./language";

/** applyPlatformLanguage adopts the platform default language unless the user has chosen one. */
export async function applyPlatformLanguage(instance: i18n): Promise<void> {
  if (chosenLanguage()) return;
  try {
    const { language } = await api.getPanelLanguage();
    if (language && !chosenLanguage()) await instance.changeLanguage(language);
  } catch {
    /* the browser language stays when the default cannot be read */
  }
}
