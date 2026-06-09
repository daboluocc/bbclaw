import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

// The admin SPA is served by the adapter under /admin and embedded into the Go
// binary. base must match the serving prefix so hashed asset URLs resolve, and
// the build output is written into the adminui package for go:embed.
export default defineConfig({
  base: "/admin/",
  plugins: [vue()],
  build: {
    outDir: "../internal/adminui/dist",
    emptyOutDir: true,
  },
  server: {
    // `npm run dev` proxies API calls to a locally running adapter.
    proxy: {
      "/v1": "http://127.0.0.1:18080",
      "/healthz": "http://127.0.0.1:18080",
    },
  },
});
