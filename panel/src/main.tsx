import React from "react";
import ReactDOM from "react-dom/client";
import i18n from "./i18n";
import { applyPlatformLanguage } from "./i18n/platformLanguage";
import App from "./App";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { installPreloadReload } from "./lib/chunk";
import "./index.css";

installPreloadReload();
void applyPlatformLanguage(i18n);

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <ErrorBoundary variant="screen">
      <App />
    </ErrorBoundary>
  </React.StrictMode>,
);
