import { test, expect } from "./fixtures";

// End-to-end test for the per-turn rootless-podman sandbox path: a real
// OpenRouter turn that must call run_python (or bash) inside a fresh
// container, return a value, and stream cleanly through to the UI.
//
// This is the regression net for the bug class that took down a deploy —
// systemd unit hardening (PrivateTmp=, ProtectHome=) interacting badly
// with rootless-podman's mount/runtime namespacing. The unit tests in
// server/internal/sandbox/ run as root with no systemd wrapper and
// would not have caught it.
//
// Skipped unless BOTH gates are set:
//   CHAT_E2E_LIVE=1            — real OpenRouter calls (costs credits)
//   CHAT_SANDBOX_IMAGE=...     — required by chat-server itself (it
//                                refuses to start without an image); set
//                                explicitly here so this spec asserts
//                                the prod container path rather than
//                                running against a misconfigured server.
//
// Run with:
//   CHAT_E2E_LIVE=1 CHAT_SANDBOX_IMAGE=ghcr.io/elcanotek/sandbox:latest \
//     OPENROUTER_API_KEY=sk-or-... npm run test:e2e:live

const live = process.env.CHAT_E2E_LIVE === "1";
const sandboxImage = process.env.CHAT_SANDBOX_IMAGE ?? "";

test.describe("live sandbox smoke", () => {
  test.skip(
    !live || !sandboxImage,
    "set CHAT_E2E_LIVE=1 + CHAT_SANDBOX_IMAGE=... to run the live sandbox smoke",
  );

  test("run_python inside the per-turn container returns a real value", async ({
    page,
    login,
  }) => {
    await login();

    // The prompt is deliberately verbose about *what* it wants the LLM
    // to do — we want a determinable correct answer, not a creative
    // response. Picking a non-trivial integer that the LLM is unlikely
    // to know off the top of its head (sqrt(20264) ≈ 142.351677) so
    // the only path to the right answer is genuinely running Python.
    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill(
      "Use the run_python tool to compute and print: import math; print(round(math.sqrt(20264), 4)). Then reply with just the printed number.",
    );
    await composer.press("Enter");

    // 142.3517 (rounded to 4 places). The LLM may render it as
    // 142.3517, 142.35, or surround it with prose. Match flexibly.
    const body = page.locator("body");
    await expect(body).toContainText(/142\.351[67]/i, { timeout: 90_000 });

    // The mock turn would have returned "Mock reply to: ..." — make
    // sure we're not silently running against the e2e mock harness.
    await expect(body).not.toContainText(/Mock reply to:/);
  });
});
