import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import path from "path";

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      "~": path.resolve(__dirname, "./src"),
    },
  },
  build: {
    rollupOptions: {
      output: {
        // Vite 8 / rolldown requires manualChunks as a function rather than
        // the Rollup-style object map. Split stable vendor code into its own
        // cacheable chunks so app code changes don't bust the cache.
        manualChunks(id) {
          if (!id.includes("node_modules")) return undefined;
          if (
            id.includes("/node_modules/react/") ||
            id.includes("/node_modules/react-dom/") ||
            id.includes("/node_modules/scheduler/")
          ) {
            return "vendor-react";
          }
          if (id.includes("/node_modules/@tanstack/")) {
            return "vendor-react-query";
          }
          if (
            id.includes("/node_modules/axios/") ||
            id.includes("/node_modules/form-data/") ||
            id.includes("/node_modules/proxy-from-env/")
          ) {
            return "vendor-axios";
          }
          if (id.includes("/node_modules/zod/")) {
            return "vendor-zod";
          }
          if (id.includes("/node_modules/lucide-react/")) {
            return "vendor-lucide";
          }
          return undefined;
        },
      },
    },
  },
  // Same-origin proxy for the API: every /api/* request from the SPA
  // is forwarded to the backend container. This makes dev match prod
  // (k8s ingress already does this) so httpOnly __Host- cookies work
  // identically in both environments. Without this, the SPA would
  // call the API on a different origin (localhost:8080 vs :5173) and
  // the browser would refuse to attach the cookies on cross-site
  // XHR.
  server: {
    proxy: {
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: false,
      },
    },
  },
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: "./src/test/setup.js",
    css: true,
    exclude: ["**/node_modules/**", "**/tests/e2e/**"],
  },
});
