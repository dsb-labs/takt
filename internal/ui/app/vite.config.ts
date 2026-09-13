import tailwindcss from "@tailwindcss/vite";
import vue from "@vitejs/plugin-vue";
import { defineConfig } from "vite";

// The bundle is built into ../dist, which the Go side embeds. The dev server
// proxies API requests to a locally running takt server, so `yarn dev` works
// against `make dev` without a build.
export default defineConfig({
  plugins: [vue(), tailwindcss()],
  // Modules are imported as "@/lib/format" rather than by counting the
  // directories back to src, which a view three levels down gets wrong
  // easily and reads badly when it gets it right.
  resolve: {
    alias: { "@": new URL("./src", import.meta.url).pathname },
  },
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
