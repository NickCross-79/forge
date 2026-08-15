import { defineConfig } from "vitest/config";

// Vitest config is separate from vite.config.ts so the production build's type
// definitions do not have to know about test-only options.
export default defineConfig({
  test: {
    environment: "node",
    include: ["src/**/*.test.ts"],
  },
});
