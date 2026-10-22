import { defineConfig, devices } from "@playwright/test";
import { getTestAuthKey } from "./e2e/test-auth-key";

// Playwright config for the unified Fleet frontend.
//
// The CI suite is MOCKED-ONLY and deterministic: it drives the real browser
// tier (the Go gateway serving the built SPA) but every backend call is
// replaced by Playwright route interception. No live OpenRouter, no Podman, no database —
// so the suite is fully reproducible on a clean ubuntu-latest runner.
//
//   testDir: ./e2e/mocked   → the deterministic, CI-runnable suite.
//
// The LIVE specs at the top level of e2e/ (auth.spec.ts, chat.spec.ts, …) drive
// a REAL booted fleet (chat-server + orchestrator in CHAT_MOCK_MODE, started by
// scripts/e2e-boot-server.sh). They are intentionally NOT part of the default
// testDir and never run in CI — see web/e2e/README.md for how to run them
// locally against a live stack.

// The browser tier's port (FLEET_WEB_ADDR) for both lanes.
const WEB_PORT = Number(process.env.WEB_PORT ?? 3100);

// Shared auth values used by the mocked specs + the gateway. These are
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
// file outside the repo, so the gateway process and the spec-worker
// processes (which both re-load this config) read the SAME material:
//   - the PUBLIC key is exported to the gateway via AUTH_SIGNING_PUBKEY
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

// The mocked suite drives the REAL browser tier: the Go gateway
// (internal/webui) serving the embedded production bundle, booted alone by
// cmd/web-e2e-server with no backends behind it. The command builds the
// bundle and the harness first (E2E_SKIP_BUILD=1 reuses both), so the suite
// always validates what ships — there is no separate dev-server mode.
const mockedServerCommand =
  process.env.E2E_SKIP_BUILD === "1"
    ? "../.e2e-bin/web-e2e-server"
    : "npm run build && cd .. && go build -o .e2e-bin/web-e2e-server ./cmd/web-e2e-server && .e2e-bin/web-e2e-server";

// E2E_LIVE selects the REAL-backend suite (e2e/live/*.spec.ts). In that mode the
// whole stack — Postgres, both Go listeners, SSE, the scheduler + worker pool,
// and the Podman sandbox — is booted for real by scripts/e2e-boot-server.sh;
// only the LLM is stubbed by the wire-compatible fake (cmd/fake-llm), reached
// via OPENROUTER_BASE_URL. The boot script owns the web listener too (the fleet binary), so in
// live/canary mode Playwright launches the boot script as its webServer.
//
// E2E_CANARY is the same real stack but with the FAKE swapped for a REAL cheap
// OpenRouter model (drift canary, secret-gated, never a PR gate). The boot
// script reads E2E_CANARY itself; here it just selects the canary project +
// the boot webServer.
const canaryMode = process.env.E2E_CANARY === "1";
const liveMode = process.env.E2E_LIVE === "1" || canaryMode;

// E2E_SCREENSHOTS=1 selects the docs-screenshot suite (e2e/screenshots/*): the
// SAME mocked gateway as the default lane, but a separate project so the
// screenshot capture never runs on (or gates) the PR mocked suite. The
// screenshot workflow (#487) invokes it with `--project=screenshots` on push to
// main and commits the captured PNGs.
const screenshotMode = process.env.E2E_SCREENSHOTS === "1";

// The live suite boots the whole real stack (the fleet binary serves the web tier) via
// scripts/e2e-boot-server.sh, which is repo-root-relative; Playwright runs from
// web/, so the command reaches up one directory.
const liveWebServer = {
  command: "bash ../scripts/e2e-boot-server.sh",
  url: `http://127.0.0.1:${WEB_PORT}/login`,
  // The boot includes a go build, an (optional) 1.3GB sandbox image build, a
  // sandbox probe, and the web bundle build — generous timeout, never a sleep.
  timeout: 900_000,
  reuseExistingServer: !process.env.CI,
  stdout: "pipe" as const,
  stderr: "pipe" as const,
  env: {
    WEB_PORT: String(WEB_PORT),
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
  // The gateway alone (see mockedServerCommand). The /api/* layer is
  // route-intercepted by Playwright; an unintercepted call answers 502.
  command: mockedServerCommand,
  url: `http://127.0.0.1:${WEB_PORT}/login`,
  timeout: 300_000,
  reuseExistingServer: !process.env.CI,
  stdout: "pipe" as const,
  stderr: "pipe" as const,
  env: {
    FLEET_WEB_ADDR: `127.0.0.1:${WEB_PORT}`,
    APP_SESSION_SECRET: TEST_SESSION_SECRET,
    AUTH_SIGNING_PUBKEY: TEST_AUTH_PUBKEY,
    FLEET_SERVER_TOKEN: "e2e-shared-secret",
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
    baseURL: `http://127.0.0.1:${WEB_PORT}`,
    trace: "retain-on-failure",
    video: "off",
  },

  // Projects are mode-scoped so a run only ever contains the matching suite:
  //   default          → the "mocked" project (the fast CI layer),
  //   E2E_LIVE=1        → the real-backend "live" project.
  // This keeps `--project=live` from silently running against the mocked server
  // (and vice-versa): the live project simply isn't present unless E2E_LIVE=1.
  projects: screenshotMode
    ? [
        {
          name: "screenshots",
          testDir: "./e2e/screenshots",
          use: { ...devices["Desktop Chrome"], viewport: { width: 1440, height: 900 } },
        },
      ]
    : canaryMode
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
            // Named "mocked" to match the suite's vocabulary (mocked/live/canary)
            // and the documented `--project=mocked` invocation; it pins the
            // deterministic e2e/mocked suite as the default CI lane.
            name: "mocked",
            testDir: "./e2e/mocked",
            use: { ...devices["Desktop Chrome"] },
          },
        ],

  // Pick the webServer by mode: the live boot script when E2E_LIVE=1, else the
  // mocked gateway.
  webServer: liveMode ? [liveWebServer] : [mockedWebServer],
});

export { TEST_EMAIL, TEST_PASSWORD, TEST_SESSION_SECRET, TEST_AUTH_PUBKEY, TEST_AUTH_PRIVATE_KEY_PEM };
