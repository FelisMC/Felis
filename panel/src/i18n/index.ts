import i18next, { type InitOptions } from "i18next";
import { initReactI18next } from "react-i18next";
import LanguageDetector from "i18next-browser-languagedetector";
import enCommon from "./resources/en-US/common.json";
import enAuth from "./resources/en-US/auth.json";
import enDashboard from "./resources/en-US/dashboard.json";
import enServers from "./resources/en-US/servers.json";
import enAccount from "./resources/en-US/account.json";
import enAdmin from "./resources/en-US/admin.json";
import enOps from "./resources/en-US/ops.json";
import enErrors from "./resources/en-US/errors.json";
import enNavigation from "./resources/en-US/navigation.json";
import enBackups from "./resources/en-US/backups.json";
import enSubmissions from "./resources/en-US/submissions.json";
import enFiles from "./resources/en-US/files.json";
import zhCommon from "./resources/zh-CN/common.json";
import zhAuth from "./resources/zh-CN/auth.json";
import zhDashboard from "./resources/zh-CN/dashboard.json";
import zhServers from "./resources/zh-CN/servers.json";
import zhAccount from "./resources/zh-CN/account.json";
import zhAdmin from "./resources/zh-CN/admin.json";
import zhOps from "./resources/zh-CN/ops.json";
import zhErrors from "./resources/zh-CN/errors.json";
import zhNavigation from "./resources/zh-CN/navigation.json";
import zhBackups from "./resources/zh-CN/backups.json";
import zhSubmissions from "./resources/zh-CN/submissions.json";
import zhFiles from "./resources/zh-CN/files.json";
import { panelLanguage, SUPPORTED_LANGUAGES } from "./language";

// Exported so a test can boot a fresh instance with the same detection.
export const i18nOptions: InitOptions = {
  resources: {
    "en-US": {
      common: enCommon,
      auth: enAuth,
      dashboard: enDashboard,
      servers: enServers,
      account: enAccount,
      admin: enAdmin,
      ops: enOps,
      errors: enErrors,
      navigation: enNavigation,
      backups: enBackups,
      submissions: enSubmissions,
      files: enFiles,
    },
    "zh-CN": {
      common: zhCommon,
      auth: zhAuth,
      dashboard: zhDashboard,
      servers: zhServers,
      account: zhAccount,
      admin: zhAdmin,
      ops: zhOps,
      errors: zhErrors,
      navigation: zhNavigation,
      backups: zhBackups,
      submissions: zhSubmissions,
      files: zhFiles,
    },
  },
  supportedLngs: [...SUPPORTED_LANGUAGES],
  fallbackLng: "en-US",
  defaultNS: "common",
  interpolation: {
    escapeValue: false,
  },
  detection: {
    order: ["localStorage", "navigator"],
    caches: ["localStorage"],
    lookupLocalStorage: "felis-lang",
    convertDetectedLanguage: panelLanguage,
  },
};

i18next.use(LanguageDetector).use(initReactI18next).init(i18nOptions);

export default i18next;
