import { describe, expect, it } from "vitest";
import { deriveConversationTitle } from "./title";

describe("deriveConversationTitle", () => {
  it("defaults to 'New chat' when there's no user message yet", () => {
    expect(deriveConversationTitle([])).toBe("New chat");
    expect(deriveConversationTitle([{ role: "assistant", content: "hi" }])).toBe("New chat");
  });

  it("uses the first user message verbatim when short enough", () => {
    expect(deriveConversationTitle([{ role: "user", content: "hello" }])).toBe("hello");
  });

  it("ignores later user messages — only the first matters", () => {
    const title = deriveConversationTitle([
      { role: "user", content: "first" },
      { role: "assistant", content: "ok" },
      { role: "user", content: "second" },
    ]);
    expect(title).toBe("first");
  });

  it("truncates long first messages and suffixes with an ellipsis", () => {
    const long = "a".repeat(100);
    const title = deriveConversationTitle([{ role: "user", content: long }]);
    expect(title.length).toBeLessThanOrEqual(47);
    expect(title.endsWith("...")).toBe(true);
  });
});
