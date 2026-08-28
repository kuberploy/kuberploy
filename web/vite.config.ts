import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": new URL("./src", import.meta.url).pathname,
    },
  },
  server: {
    port: 4173,
    proxy: {
      "/v1": {
        target: process.env.KUBERPLOY_API_DEV_URL ?? "http://127.0.0.1:8080",
        changeOrigin: true,
      },
    },
  },
  test: {
    environment: "jsdom",
    setupFiles: "./src/test/setup.ts",
    restoreMocks: true,
    // Complex user-event flows can exceed Vitest's 5s default when the full
    // browser-facing suite runs in parallel. Let each awaited interaction
    // finish so a timed-out keyboard sequence cannot leak into the next test.
    testTimeout: 15_000,
  },
});
