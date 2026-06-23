import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

// The adapter_v2 web SPA is served by the Go binary at the site root ("/") and
// embedded via `//go:embed all:dist` (see web/web.go). base must be "/" so the
// hashed asset URLs resolve at the root, and the build output is written into
// web/dist for go:embed to pick up. The dist tree is committed (the Go build
// needs it at compile time and ships with no Node toolchain).
export default defineConfig({
  base: "/",
  root: ".",
  plugins: [vue()],
  build: {
    outDir: "../dist",
    emptyOutDir: true,
  },
  server: {
    // `npm run dev` proxies API + WS calls to a locally running adapter.
    proxy: {
      "/v1": "http://127.0.0.1:18090",
      "/healthz": "http://127.0.0.1:18090",
      "/ws": {
        target: "ws://127.0.0.1:18090",
        ws: true,
      },
    },
  },
});
