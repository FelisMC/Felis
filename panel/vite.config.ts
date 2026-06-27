import { defineConfig, type PluginOption } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath, URL } from "node:url";

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
    server: {
      proxy: {
        "/api": { target: "http://localhost:8080", changeOrigin: true },
      },
    },
  };
});
