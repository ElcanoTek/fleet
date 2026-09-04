#!/usr/bin/env node
// Record the current web UI with scripted data, without credentials or a model.
// Playwright owns server startup, authentication, assertions and video capture.
import { spawnSync } from "node:child_process";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
const result = spawnSync("npx", ["playwright", "test", "--project=screenshots", "demos.spec.ts"], {
  cwd: resolve(root, "web"),
  stdio: "inherit",
  env: { ...process.env, E2E_SCREENSHOTS: "1", FLEET_RECORD_DEMOS: "1" },
});
if (result.error) console.error(result.error.message);
process.exit(result.status ?? 1);
