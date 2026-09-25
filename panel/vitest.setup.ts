import { afterEach } from "vitest";
import i18next from "i18next";
import "./src/i18n";

i18next.changeLanguage("en-US");

// Component tests opt into jsdom per file (`// @vitest-environment jsdom`);
// the lib tests stay on node. Testing Library only unmounts between tests on
// its own when vitest globals are on, so do it here, and only where a DOM exists.
afterEach(async () => {
  if (typeof document === "undefined") return;
  const { cleanup } = await import("@testing-library/react");
  cleanup();
});
