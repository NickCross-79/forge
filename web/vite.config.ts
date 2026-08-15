import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The dashboard is embedded into the forge binary from web/dist, so the build
// output goes there and asset paths stay relative to the server root.
//
// In development, `npm run dev` serves the UI on :5173 and proxies API calls to
// a `forge serve` on :7777, which keeps hot reload working without CORS
// gymnastics. The Go server also allows the dev origin explicitly.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": { target: "http://127.0.0.1:7777", changeOrigin: true },
      "/metrics": { target: "http://127.0.0.1:7777", changeOrigin: true },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // Source maps would roughly double the embedded payload for little benefit
    // in a local tool.
    sourcemap: false,
  },
});
