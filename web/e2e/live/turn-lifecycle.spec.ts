import { test, expect } from "./fixtures";

// LIVE turn journeys the tool-loop spec does not reach, each driven by a
// fake-LLM scenario (cmd/fake-llm) against the fully real stack:
//
//   file-tools     — write_file, bash and view_file share ONE sandbox
//                    workspace, both ways (#784: the file tools run through the
//                    sandbox FileOp seam, never on the host).
//   stop-turn      — Stop really ends the server-side turn: the model stalls,
//                    the user stops it, and a follow-up message gets a reply.
//                    The chat server queues input behind a live turn, so that
//                    reply can only arrive once the stopped turn has ended.
//   provider-error — a provider that rejects the request fails the turn
//                    visibly instead of leaving a spinner.

function conversationRegion(page: import("@playwright/test").Page) {
  return page.getByRole("region", { name: "Conversation" });
}

async function send(page: import("@playwright/test").Page, text: string) {
  const composer = page.getByPlaceholder(/message .* ai/i);
  await composer.fill(text);
  await composer.press("Enter");
}

test.describe("live turn lifecycle (real chat server + sandbox)", () => {
  test("write_file, bash and view_file see the same sandbox workspace", async ({ page, login }) => {
    // Cold container start plus three sandboxed tool calls.
    test.setTimeout(150_000);
    await login();
    await send(page, "round-trip a file through the sandbox [[scenario:file-tools]]");

    const conversation = conversationRegion(page);
    // The final text is emitted only after all three tools have run and their
    // results were fed back, so it proves the loop completed.
    await expect(conversation).toContainText(/File tools round trip complete\./, { timeout: 90_000 });
    await expect(conversation).not.toContainText(/Mock reply to:/i);

    // bash read back what write_file wrote (FileOp → container workspace).
    const bashChip = page.getByRole("button", { name: /bash/i }).first();
    await expect(bashChip).toBeVisible({ timeout: 15_000 });
    await bashChip.click();
    await expect(page.locator("pre", { hasText: "FAKELLM_WRITE_FILE_OK" }).first()).toBeVisible({
      timeout: 10_000,
    });

    // view_file read back what bash wrote (container workspace → FileOp). The
    // 42 is computed by `expr` in the sandbox, so this is real sandbox output.
    // (The final text above renders even when a tool fails — e.g. the bash
    // guard refusing a construct — so these per-tool reads are the proof.)
    const viewChip = page.getByRole("button", { name: /view_file/i }).first();
    await viewChip.click();
    await expect(page.locator("pre", { hasText: "FAKELLM_BASH_WROTE_42" }).first()).toBeVisible({
      timeout: 10_000,
    });
  });

  test("Stop ends a stalled turn and the conversation takes the next message", async ({ page, login }) => {
    test.setTimeout(120_000);
    await login();
    await send(page, "start a turn that stalls [[scenario:stop-turn]]");

    const stop = page.getByRole("button", { name: "Stop generating" });
    await expect(stop).toBeVisible({ timeout: 30_000 });
    await stop.click();

    const conversation = conversationRegion(page);
    await expect(conversation).toContainText(/Turn stopped\./, { timeout: 30_000 });
    await expect(stop).toBeHidden();

    // The stopped turn must not finish in the background...
    await expect(conversation).not.toContainText("FAKELLM_STOP_TURN_SHOULD_NEVER_RENDER");
    // ...and must have released the conversation: a stalled turn still holding
    // it queues this message behind a ten-minute provider call (checked: with
    // no Stop, no reply arrives in 20s). The reply normally lands in about a
    // second; the timeout stays well short of any stall watchdog that could
    // end the turn on its own and make this pass without Stop working.
    await send(page, "are you still there? [[echo:FAKELLM_AFTER_STOP_OK]]");
    await expect(conversation).toContainText("FAKELLM_AFTER_STOP_OK", { timeout: 15_000 });
  });

  test("a provider that rejects the request fails the turn visibly", async ({ page, login }) => {
    test.setTimeout(120_000);
    await login();
    await send(page, "this request will be rejected [[scenario:provider-error]]");

    const conversation = conversationRegion(page);
    await expect(conversation).toContainText(/The selected model returned an error \(HTTP 400\)/, {
      timeout: 60_000,
    });
    await expect(conversation).toContainText(/Turn failed/);
    await expect(page.getByRole("button", { name: "Stop generating" })).toBeHidden();
  });
});
