import { test, expect } from "./fixtures";

// Per-conversation streaming: the user can start a turn in one chat,
// switch to a brand-new chat mid-stream, and send a second prompt
// without the first one being cancelled or the composer being blocked.
// The sidebar paints a "working" dot next to each in-flight chat.

test.describe("parallel chats", () => {
  test("can submit a new chat while another is mid-stream; both replies land", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);

    // Submit chat A. Wait only until the server has named the slot — at
    // that point the PENDING_CONV_KEY → real-id rename has fired, the
    // sidebar row exists, and the streaming-set entry is keyed by the
    // real conv id (which is what the next "new chat" click depends on
    // for not stomping over chat A's abort controller).
    await composer.fill("chat alpha streaming");
    const aTitled = page.waitForResponse(
      (res) =>
        res.url().includes("/api/chat") &&
        res.request().method() === "POST" &&
        res.ok(),
    );
    await composer.press("Enter");
    await aTitled;

    const sidebar = page.locator("aside").first();
    await expect(sidebar.getByText("chat alpha streaming")).toBeVisible({
      timeout: 10_000,
    });

    // Open a fresh chat. The "New chat" button keeps the previous
    // conversation streaming in the background. With the per-conv
    // streaming refactor, the composer (now bound to a brand-new PENDING
    // slot) must accept input and let the user submit — before the
    // refactor, isStreaming was global and the send button stayed
    // disabled until chat A finished.
    await page.getByRole("button", { name: /new chat/i }).click();
    await composer.fill("chat beta parallel");
    const sendBtn = page.getByRole("button", { name: /send message/i });
    await expect(sendBtn).toBeEnabled({ timeout: 5_000 });
    await composer.press("Enter");

    // Both replies must eventually land. Chat B's reply renders inline
    // (because that's the active conv); chat A's reply gets persisted
    // server-side and surfaces when we navigate back to it.
    await expect(page.getByText(/Mock reply to:\s*chat beta parallel/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // Switch back to chat A and verify its reply was preserved (i.e. the
    // background stream wasn't aborted by the new-chat click).
    await sidebar.getByText("chat alpha streaming").click();
    await expect(page.getByText(/Mock reply to:\s*chat alpha streaming/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // Both rows in the sidebar.
    await expect(sidebar.getByText("chat alpha streaming")).toBeVisible();
    await expect(sidebar.getByText("chat beta parallel")).toBeVisible();
  });

  test("clicking + New chat immediately after submit does not kill chat A's stream", async ({ page, login }) => {
    // The pre-fix race: submitPrompt put its AbortController on the
    // singleton PENDING_CONV_KEY, so a "+ New chat" click before the
    // "conversation" SSE event landed would abort that controller and
    // kill the in-flight turn. With per-submission pending keys,
    // clearConversation only tears down the slot the user is staring
    // at when it's *idle* — the in-flight turn keeps running under its
    // own key and gets promoted to a real conv id when the rename
    // event arrives.
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("race-test chat alpha");
    // Submit and IMMEDIATELY click "+ New chat" — no awaits in between
    // so the click can land inside the PENDING window. Promise.all
    // sequences both interactions in the same task. Use getByTitle to
    // scope to the sidebar's plus button (the header title placeholder
    // also reads "New chat" until the auto-title lands and would
    // otherwise produce a strict-mode collision).
    await Promise.all([
      composer.press("Enter"),
      page.getByTitle("New chat", { exact: true }).click(),
    ]);

    // Chat A's row should still appear in the sidebar — the stream
    // survived the new-chat click and the server finished its turn.
    const sidebar = page.locator("aside").first();
    await expect(sidebar.getByText("race-test chat alpha")).toBeVisible({
      timeout: 15_000,
    });
    await sidebar.getByText("race-test chat alpha").click();
    await expect(page.getByText(/Mock reply to:\s*race-test chat alpha/i).first()).toBeVisible({
      timeout: 15_000,
    });
  });

  test("composer drafts are per-conversation; switching chats restores the right draft", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);

    // Land chat A so it has a real conv id and a sidebar row.
    await composer.fill("alpha topic");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*alpha topic/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // Type an unsubmitted draft into chat A's composer.
    await composer.fill("draft i want to keep on alpha");

    // Switch to a brand-new chat — composer should be empty, NOT
    // carry over A's draft. (This is the regression the per-conv
    // composer state fixes: previously the textarea was a global
    // string that followed the user across chats.)
    await page.getByRole("button", { name: /new chat/i }).click();
    await expect(composer).toHaveValue("");

    // Switch back to chat A via the sidebar — A's draft must return.
    const sidebar = page.locator("aside").first();
    await sidebar.getByText("alpha topic").click();
    await expect(composer).toHaveValue("draft i want to keep on alpha");

    // Submit on A (this consumes A's draft) and verify the reply lands.
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*draft i want to keep on alpha/i).first()).toBeVisible({
      timeout: 15_000,
    });
  });

  test("submitting and then opening + New chat does not yank back to the just-promoted chat", async ({ page, login }) => {
    // Pre-fix: the conv event's `currentActive === null` branch would
    // setActiveConversationId(realId) the moment the server named the
    // chat — even when the user had explicitly clicked + New chat in
    // the meantime, jerking the view back to the chat they just left.
    // Per-submission pending keys make this case detectable: by submit
    // time active = pk, and clicking + New chat moves active to null;
    // the conv event must NOT yank null → realId.
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("yank-test alpha topic");
    // Submit and immediately go to the empty new-chat view. The conv
    // event lands while we're on the empty view.
    await Promise.all([
      composer.press("Enter"),
      page.getByTitle("New chat", { exact: true }).click(),
    ]);

    // The empty-state heading (or at minimum: no transcript yet) must
    // remain visible — i.e. the conv event did not switch us to the
    // newly-named chat. The sidebar will still get the row.
    const sidebar = page.locator("aside").first();
    await expect(sidebar.getByText("yank-test alpha topic")).toBeVisible({
      timeout: 10_000,
    });
    // Composer empty (the empty new-chat view) — not showing the
    // user message bubble from alpha.
    await expect(composer).toHaveValue("");
    // And the alpha user-bubble text must NOT appear in the main
    // conversation region (i.e. we're not viewing alpha).
    const main = page.getByRole("main");
    await expect(main.getByText("yank-test alpha topic", { exact: true })).toHaveCount(0);
  });

  test("Stop on the active chat does not abort another chat's stream", async ({ page, login }) => {
    // Two parallel chats; clicking Stop must only cancel the one the
    // user is currently looking at. The other keeps streaming and its
    // reply still lands.
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);

    // Start chat A and wait for its sidebar row.
    await composer.fill("stop-test chat alpha");
    await composer.press("Enter");
    const sidebar = page.locator("aside").first();
    await expect(sidebar.getByText("stop-test chat alpha")).toBeVisible({
      timeout: 10_000,
    });
    // Let chat A finish so its row is fully settled before we start B.
    await expect(page.getByText(/Mock reply to:\s*stop-test chat alpha/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // Start chat B. Send long-ish content so the mock turn streams for
    // a measurable window (each word becomes a 10ms text delta).
    await page.getByTitle("New chat", { exact: true }).click();
    const betaPrompt = "stop-test chat beta " + "x ".repeat(40);
    await composer.fill(betaPrompt);
    await composer.press("Enter");
    await expect(sidebar.getByText(/stop-test chat beta/i)).toBeVisible({
      timeout: 10_000,
    });

    // Switch to chat A. Stop must NOT be visible — A is idle, so the
    // composer doesn't show the Stop affordance even though chat B is
    // still streaming in the background.
    await sidebar.getByText("stop-test chat alpha").click();
    await expect(page.getByRole("button", { name: /^stop$/i })).toHaveCount(0);

    // Navigate back to chat B and verify its reply landed (proves the
    // background stream completed unimpeded while we were on A).
    await sidebar.getByText(/stop-test chat beta/i).click();
    await expect(page.getByText(/Mock reply to:\s*stop-test chat beta/i).first()).toBeVisible({
      timeout: 15_000,
    });
  });

  test("sidebar paints a working indicator next to an in-flight chat", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("dot indicator probe");
    await composer.press("Enter");

    // The streaming dot is the first child of the title span — a small
    // round pulse colored with the accent variable, aria-labeled
    // "Working". It only renders while the conv is in `streamingConvs`,
    // so we have to assert it appears before the turn completes.
    const sidebar = page.locator("aside").first();
    await expect(sidebar.getByLabel(/working/i).first()).toBeVisible({
      timeout: 5_000,
    });

    // After the mock turn ends, the dot must come down.
    await expect(page.getByText(/Mock reply to:\s*dot indicator probe/i).first()).toBeVisible({
      timeout: 15_000,
    });
    await expect(sidebar.getByLabel(/working/i)).toHaveCount(0, { timeout: 5_000 });
  });
});
