import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import path from "path";

// Sentry plugin — conditionally registered. The plugin reads
// SENTRY_AUTH_TOKEN / SENTRY_ORG / SENTRY_PROJECT from process.env
// at build time and uploads sourcemaps to the matching project. We
// only register it when the auth token is present so local builds
// without a token still succeed (the plugin errors on a missing
// token during the upload step). Sourcemap files are deleted from
// the build output after upload so the nginx image never serves
// the original source (SECURITY.md L5).
const sentryPlugins: import("vite").PluginOption[] = [];
if (
  process.env.SENTRY_AUTH_TOKEN &&
  process.env.SENTRY_ORG &&
  process.env.SENTRY_PROJECT
) {
  const { sentryVitePlugin } = await import("@sentry/vite-plugin");
  sentryPlugins.push(
    sentryVitePlugin({
      org: process.env.SENTRY_ORG,
      project: process.env.SENTRY_PROJECT,
      authToken: process.env.SENTRY_AUTH_TOKEN,
      sourcemaps: { filesToDeleteAfterUpload: ["**/*.map"] },
    }),
  );
}

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [react(), ...sentryPlugins],
  resolve: {
    alias: {
      "~": path.resolve(__dirname, "./src"),
    },
  },
  build: {
    // Source maps stay off in production builds. The .map files would
    // land in the nginx image and let anyone fetch the original
    // source over the same origin as the SPA (XSS-grade info leak).
    // For local debugging, `npm run dev` emits inline source maps
    // and the build target is dev-only anyway. SECURITY.md L5.
    sourcemap: false,
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
  // No `/api` dev proxy anymore — the SPA now hits the backend
  // directly via VITE_API_URL and the browser handles the CORS
  // preflight. Same-origin proxying would re-introduce cookie
  // semantics; Bearer tokens don't need it. Devs see the same
  // cross-origin XHR / preflight round-trips production sees,
  // which makes dev a more faithful predictor of prod.
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: "./src/test/setup.js",
    css: true,
    exclude: ["**/node_modules/**", "**/tests/e2e/**"],
  },
});