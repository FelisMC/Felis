import { defineConfig, type PluginOption } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath, URL } from "node:url";

const FRAMEWORK =
  /[\\/]node_modules[\\/](react|react-dom|scheduler|react-router|react-router-dom|@remix-run[\\/]router|i18next|react-i18next|i18next-browser-languagedetector)[\\/]/;

// The panel is served by felis-api behind Zero-Trust; the API lives under the
// same origin in production, so /api proxies there in dev.
export default defineConfig(async ({ mode }) => {
  const plugins: PluginOption[] = [react()];
  if (mode === "mock") {
    const { mockApiPlugin } = await import("./dev/mockApi");
    plugins.unshift(mockApiPlugin());
  }

  return {
    plugins,
    resolve: {
      alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
    },
    build: {
      rollupOptions: {
        output: {
          // The framework changes far less often than the panel, so it gets a
          // chunk of its own that stays cached across panel releases. Listed by
          // name: a blanket node_modules rule would pull the 3D fleet's three.js
          // out of its lazy chunk into the first load.
          manualChunks(id: string) {
            if (FRAMEWORK.test(id)) return "framework";
          },
        },
      },
    },
    server: {
      proxy: {
        "/api": { target: "http://localhost:8080", changeOrigin: true },
      },
    },
    test: {
      setupFiles: ["./vitest.storage.ts", "./vitest.setup.ts"],
      // e2e/*.spec.ts belong to Playwright (npm run test:e2e).
      include: ["src/**/*.test.{ts,tsx}"],
    },
  };
});
