import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import path from "node:path";

// Vitest config — scoped to unit tests of UI helpers and pure modules.
// Playwright e2e tests live in /e2e and use their own runner.
export default defineConfig({
  plugins: [react()],
  test: {
    environment: "jsdom",
    globals: true,
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    setupFiles: ["./vitest.setup.ts"],
    // Every case starts from the real globals and real spied-on methods:
    // vi.stubGlobal (fetch, matchMedia, …) is undone and vi.spyOn spies are
    // restored after each test, so a stub can never leak into the next case
    // or file and no test needs its own afterEach to undo them.
    unstubGlobals: true,
    restoreMocks: true,
  },
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
});
