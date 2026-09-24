import React from "react";
import ReactDOM from "react-dom/client";
import "./i18n";
import App from "./App";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { installPreloadReload } from "./lib/chunk";
import "./index.css";

installPreloadReload();

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <ErrorBoundary variant="screen">
      <App />
    </ErrorBoundary>
  </React.StrictMode>,
);
