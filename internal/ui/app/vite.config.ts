import tailwindcss from "@tailwindcss/vite";
import vue from "@vitejs/plugin-vue";
import { defineConfig } from "vite";

// The bundle is built into ../dist, which the Go side embeds. The dev server
// proxies API requests to a locally running orca server, so `yarn dev` works
// against `make dev` without a build.
export default defineConfig({
  plugins: [vue(), tailwindcss()],
  build: {
    outDir: "../dist",
    emptyOutDir: true,
  },
  server: {
    proxy: {
      "/api": "http://localhost:7373",
    },
  },
});
