import { defineConfig, devices } from "@playwright/test";
import { getTestAuthKey } from "./e2e/test-auth-key";

// Playwright config for the unified Fleet frontend.
//
// The CI suite is MOCKED-ONLY and deterministic: it drives the real Next.js app
// but every Go backend (chat-server :18080, orchestrator/moc :8000) is replaced
// by Playwright route interception. No live OpenRouter, no Podman, no database —
// so the suite is fully reproducible on a clean ubuntu-latest runner.
//
//   testDir: ./e2e/mocked   → the deterministic, CI-runnable suite.
//
// The LIVE specs at the top level of e2e/ (auth.spec.ts, chat.spec.ts, …) drive
// a REAL booted fleet (chat-server + orchestrator in CHAT_MOCK_MODE, started by
// scripts/e2e-boot-server.sh). They are intentionally NOT part of the default
// testDir and never run in CI — see web/e2e/README.md for how to run them
// locally against a live stack.

const NEXT_PORT = Number(process.env.NEXT_PORT ?? 3100);

// Shared auth values used by the mocked specs + the Next server. These are
// throwaway test secrets — never real credentials.
const TEST_EMAIL = "e2e@example.com";
const TEST_PASSWORD = "e2e-test-password";
const TEST_SESSION_SECRET = "e2e-session-secret-0123456789abcdef";

// A throwaway Ed25519 keypair, GENERATED FRESH AT RUNTIME (see
// e2e/test-auth-key.ts), so the "Use Elcano email" path renders and the
// elcano_auth (magic-link) cookie can be minted + verified end-to-end. NO key
// literal is committed to the repo.
//
// The keypair is generated exactly ONCE per run and persisted to a throwaway
// file outside the repo, so the Next server process and the spec-worker
// processes (which both re-load this config) read the SAME material:
//   - the PUBLIC key is exported to the Next server via AUTH_SIGNING_PUBKEY
//     (standard base64 of the raw 32-byte key, matching auth-admin keygen and
//     home/server.js's `Buffer.from(AUTH_SIGNING_PUBKEY, "base64")`), so the
//     real verifier (verifyElcanoToken) trusts tokens signed by this keypair.
//   - the matching PRIVATE key (PKCS8 PEM) is read by the mocked test helper
//     (e2e/mocked/_session.ts) so a spec can mint a valid elcano_auth cookie the
//     way the real auth service would.
// Server + signer always agree, and neither value protects anything real.
const testAuthKey = getTestAuthKey();
const TEST_AUTH_PUBKEY = testAuthKey.pubkeyStdB64;
const TEST_AUTH_PRIVATE_KEY_PEM = testAuthKey.privateKeyPem;

// In CI we BUILD the app and run `next start` (production server) so the suite
// validates the shipped build, not just the dev server. Locally we default to
// `next dev` for fast iteration; set E2E_PROD=1 to mirror CI.
const useProdServer = process.env.CI || process.env.E2E_PROD === "1";
const serverCommand = useProdServer
  ? `next start -p ${NEXT_PORT} -H 127.0.0.1`
  : `next dev -p ${NEXT_PORT} -H 127.0.0.1`;

// E2E_LIVE selects the REAL-backend suite (e2e/live/*.spec.ts). In that mode the
// whole stack — Postgres, both Go listeners, SSE, the scheduler + worker pool,
// and the Podman sandbox — is booted for real by scripts/e2e-boot-server.sh;
// only the LLM is stubbed by the wire-compatible fake (cmd/fake-llm), reached
// via OPENROUTER_BASE_URL. The boot script owns the Next server too, so in
// live/canary mode Playwright launches the boot script as its webServer.
//
// E2E_CANARY is the same real stack but with the FAKE swapped for a REAL cheap
// OpenRouter model (drift canary, secret-gated, never a PR gate). The boot
// script reads E2E_CANARY itself; here it just selects the canary project +
// the boot webServer.
const canaryMode = process.env.E2E_CANARY === "1";
const liveMode = process.env.E2E_LIVE === "1" || canaryMode;

// The live suite boots the whole real stack (incl. the Next server) via
// scripts/e2e-boot-server.sh, which is repo-root-relative; Playwright runs from
// web/, so the command reaches up one directory.
const liveWebServer = {
  command: "bash ../scripts/e2e-boot-server.sh",
  url: `http://127.0.0.1:${NEXT_PORT}/login`,
  // The boot includes a go build, an (optional) 1.3GB sandbox image build, a
  // sandbox probe, and a full `next build` — generous timeout, never a sleep.
  timeout: 900_000,
  reuseExistingServer: !process.env.CI,
  stdout: "pipe" as const,
  stderr: "pipe" as const,
  env: {
    NEXT_PORT: String(NEXT_PORT),
    // Forward the sandbox image + DB DSN the CI job resolves, when present.
    ...(process.env.FLEET_SANDBOX_IMAGE
      ? { FLEET_SANDBOX_IMAGE: process.env.FLEET_SANDBOX_IMAGE }
      : {}),
    ...(process.env.E2E_DATABASE_DSN ? { E2E_DATABASE_DSN: process.env.E2E_DATABASE_DSN } : {}),
    // Canary: tell the boot script to use the REAL OpenRouter + forward the key.
    ...(canaryMode ? { E2E_CANARY: "1" } : {}),
    ...(canaryMode && process.env.OPENROUTER_API_KEY
      ? { OPENROUTER_API_KEY: process.env.OPENROUTER_API_KEY }
      : {}),
    ...(process.env.CANARY_MODEL ? { CANARY_MODEL: process.env.CANARY_MODEL } : {}),
  },
};

const mockedWebServer = {
  // The Next server in CHAT_MOCK_MODE. The same mock-mode contract the live
  // harness relies on stays wired; in the mocked suite the /api/* layer is
  // additionally route-intercepted by Playwright itself, so the upstream URLs
  // below are never actually reached.
  command: serverCommand,
  url: `http://127.0.0.1:${NEXT_PORT}`,
  timeout: 120_000,
  reuseExistingServer: !process.env.CI,
  stdout: "pipe" as const,
  stderr: "pipe" as const,
  env: {
    CHAT_MOCK_MODE: "1",
    APP_SESSION_SECRET: TEST_SESSION_SECRET,
    AUTH_SIGNING_PUBKEY: TEST_AUTH_PUBKEY,
    CHAT_SERVER_URL: "http://127.0.0.1:18080",
    CHAT_SERVER_TOKEN: "e2e-shared-secret",
    ORCHESTRATOR_SERVER_URL: "http://127.0.0.1:18000",
  },
};

export default defineConfig({
  // Default testDir is the mocked suite (the fast CI layer). The live project
  // overrides testDir to e2e/live; select it with `--project=live`.
  testDir: "./e2e/mocked",
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : [["list"]],

  use: {
    baseURL: `http://127.0.0.1:${NEXT_PORT}`,
    trace: "retain-on-failure",
    video: "off",
  },

  // Projects are mode-scoped so a run only ever contains the matching suite:
  //   default          → the mocked "chromium" project (the fast CI layer),
  //   E2E_LIVE=1        → the real-backend "live" project.
  // This keeps `--project=live` from silently running against the mocked server
  // (and vice-versa): the live project simply isn't present unless E2E_LIVE=1.
  projects: canaryMode
    ? [
        {
          name: "canary",
          testDir: "./e2e/canary",
          use: { ...devices["Desktop Chrome"] },
        },
      ]
    : liveMode
      ? [
          {
            name: "live",
            testDir: "./e2e/live",
            use: { ...devices["Desktop Chrome"] },
          },
        ]
      : [
          {
            name: "chromium",
            testDir: "./e2e/mocked",
            use: { ...devices["Desktop Chrome"] },
          },
        ],

  // Pick the webServer by mode: the live boot script when E2E_LIVE=1, else the
  // mocked Next server.
  webServer: liveMode ? [liveWebServer] : [mockedWebServer],
});

export { TEST_EMAIL, TEST_PASSWORD, TEST_SESSION_SECRET, TEST_AUTH_PUBKEY, TEST_AUTH_PRIVATE_KEY_PEM };
