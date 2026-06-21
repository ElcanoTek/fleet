import { defineConfig, devices } from "@playwright/test";

// Playwright config for the unified frontend (P7).
//
// P7 is MOCKED-ONLY (live E2E against real Go backends is P8). The chat
// toolchain's CHAT_MOCK_MODE=1 mechanism is preserved: when the real
// chat-server / orchestrator binaries are present (P8), e2e-boot scripts can
// start them in mock mode. In THIS web-only tree there is no Go binary, so the
// P7 suite drives the Next dev server alone and mocks every /api/* call with
// Playwright's route interception (see e2e/mocked/*.spec.ts). The legacy chat
// e2e specs that need a live chat-server are excluded from the default run via
// `testMatch` and only run under the P8 live harness.

const NEXT_PORT = Number(process.env.NEXT_PORT ?? 3100);

// Shared auth values used by the mocked specs + the Next dev server.
const TEST_EMAIL = "e2e@example.com";
const TEST_PASSWORD = "e2e-test-password";
const TEST_SESSION_SECRET = "e2e-session-secret-0123456789abcdef";
// A throwaway Ed25519 public key so the "Use Elcano email" path renders and the
// middleware can be exercised. (The mocked specs set the cookie directly via a
// signed test token; see e2e/mocked/_session.ts.)
const TEST_AUTH_PUBKEY = process.env.TEST_AUTH_PUBKEY ?? "";

export default defineConfig({
  testDir: "./e2e/mocked",
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  reporter: [["list"]],

  use: {
    baseURL: `http://127.0.0.1:${NEXT_PORT}`,
    trace: "retain-on-failure",
    video: "off",
  },

  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],

  webServer: [
    {
      // Next.js dev server only. CHAT_MOCK_MODE=1 is exported so the same
      // mock-mode contract the P8 harness relies on stays wired; in P7 the
      // /api/* layer is additionally route-mocked by Playwright itself.
      command: `next dev -p ${NEXT_PORT} -H 127.0.0.1`,
      url: `http://127.0.0.1:${NEXT_PORT}`,
      timeout: 120_000,
      reuseExistingServer: !process.env.CI,
      stdout: "pipe",
      stderr: "pipe",
      env: {
        CHAT_MOCK_MODE: "1",
        APP_SESSION_SECRET: TEST_SESSION_SECRET,
        CHAT_SERVER_URL: "http://127.0.0.1:18080",
        CHAT_SERVER_TOKEN: "e2e-shared-secret",
        ORCHESTRATOR_SERVER_URL: "http://127.0.0.1:18000",
        ...(TEST_AUTH_PUBKEY ? { AUTH_SIGNING_PUBKEY: TEST_AUTH_PUBKEY } : {}),
      },
    },
  ],
});

export { TEST_EMAIL, TEST_PASSWORD, TEST_SESSION_SECRET };
