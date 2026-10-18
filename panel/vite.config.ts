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
          // Name shared framework chunks explicitly so three.js remains lazy-loaded.
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
