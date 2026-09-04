import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { UserTurn } from "./ChatTranscript";
import type { Message } from "./history";

// QA finding #6. The injected "Shared file library (files your administrator
// published…)" block used to render inside the user's own bubble, so the
// transcript claimed the user had typed it. The bubble now holds only what
// they typed; the injected suffix hangs off the turn as a collapsed note.

const TYPED = "what is the CPM by channel?";
const INJECTED =
  "\n\n---\n**Shared file library (files your administrator published):**\n- `rate_card.csv` (12 KiB)\n";

const turn = (over: Partial<Message> = {}): Message => ({
  id: 1,
  role: "user",
  content: TYPED,
  state: "done",
  ...over,
});

const renderTurn = (message: Message) =>
  render(
    <UserTurn
      message={message}
      isLastUser={false}
      isStreaming={false}
      editRequestSignal={0}
      onResend={() => {}}
    />,
  );

describe("UserTurn", () => {
  it("keeps the bubble to what the user typed and puts injected context outside it", () => {
    renderTurn(turn({ injectedContext: INJECTED }));

    const bubble = screen.getByTestId("user-message-bubble");
    expect(bubble).toHaveTextContent(TYPED);
    expect(bubble.textContent).not.toContain("Shared file library");

    const note = screen.getByRole("button", { name: /Context fleet added/ });
    expect(bubble.contains(note)).toBe(false);
    expect(note).toHaveAttribute("aria-expanded", "false");
    // Present in the turn, just not in the bubble.
    const context = screen.getByText(/Shared file library/);
    expect(bubble.contains(context)).toBe(false);
    expect(context).not.toBeVisible();
  });

  it("renders no note at all for a turn with nothing injected", () => {
    renderTurn(turn());
    expect(screen.getByTestId("user-message-bubble")).toHaveTextContent(TYPED);
    expect(
      screen.queryByRole("button", { name: /Context fleet added/ }),
    ).toBeNull();
  });

  // Rows written before migration 056 keep the blocks inside content.text and
  // carry no injected_context. They must render exactly as they always did —
  // no note, and no half-parsed text.
  it("leaves a legacy row exactly as it was", () => {
    const legacy = `${TYPED}\n\n---\n**User attached files:**\n- \`spend.csv\``;
    renderTurn(turn({ content: legacy }));
    const bubble = screen.getByTestId("user-message-bubble");
    expect(bubble).toHaveTextContent(/what is the CPM by channel\?/);
    expect(bubble).toHaveTextContent(/User attached files/);
    expect(
      screen.queryByRole("button", { name: /Context fleet added/ }),
    ).toBeNull();
  });
});
