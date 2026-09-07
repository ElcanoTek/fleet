import { describe, expect, it } from "vitest";
import { conversationApiUrl, isConversationId } from "./conversationApiUrl";

const uuid = "550e8400-e29b-41d4-a716-446655440000";

describe("isConversationId", () => {
  it("accepts a RFC 4122 UUID and other single path-safe tokens", () => {
    expect(isConversationId(uuid)).toBe(true);
    expect(isConversationId(uuid.toUpperCase())).toBe(true);
    expect(isConversationId("conv-1")).toBe(true);
  });

  it("rejects path traversal, extra segments, and pending keys", () => {
    expect(isConversationId("../pastebin/123")).toBe(false);
    expect(isConversationId(`${uuid}/../admin`)).toBe(false);
    expect(isConversationId("__pending__:1")).toBe(false);
    expect(isConversationId("")).toBe(false);
    expect(isConversationId("..")).toBe(false);
  });
});

describe("conversationApiUrl", () => {
  it("builds a same-origin path with the id as one encoded segment", () => {
    expect(conversationApiUrl(uuid)).toBe(
      `/api/conversations/${encodeURIComponent(uuid)}`,
    );
    expect(conversationApiUrl(uuid, "/inflight")).toBe(
      `/api/conversations/${encodeURIComponent(uuid)}/inflight`,
    );
    expect(
      conversationApiUrl(uuid, `/stream?turn_id=${encodeURIComponent("t1")}`),
    ).toBe(
      `/api/conversations/${encodeURIComponent(uuid)}/stream?turn_id=${encodeURIComponent("t1")}`,
    );
  });

  it("returns null for an id that could rewrite the path", () => {
    expect(conversationApiUrl("../evil")).toBeNull();
    expect(conversationApiUrl(`${uuid}/../../login`)).toBeNull();
  });
});
