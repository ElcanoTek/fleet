import { describe, expect, it } from "vitest";
import {
  conversationApiUrl,
  conversationApprovalApiUrl,
  isApiId,
  isConversationId,
} from "./conversationApiUrl";

const uuid = "550e8400-e29b-41d4-a716-446655440000";
const approvalUuid = "6fa459ea-ee8a-3ca4-894e-db77e160355e";

// Every value here could, if interpolated raw, move a same-origin request to
// another path, another query, or another host.
const hostileIds = [
  "",
  "..",
  "../x",
  "../pastebin/123",
  `${uuid}/../admin`,
  "a/b",
  "a\\b",
  "?q",
  "a?q=1",
  "a#frag",
  "%2e%2e",
  "%2e%2e%2fadmin",
  "a%2fb",
  "http://evil",
  "//evil.example",
  "https:evil",
  "a b",
  "a\nb",
  "__pending__:1",
  ".hidden",
];

describe("isApiId / isConversationId", () => {
  it("accepts a RFC 4122 UUID and other single path-safe tokens", () => {
    for (const check of [isApiId, isConversationId]) {
      expect(check(uuid)).toBe(true);
      expect(check(uuid.toUpperCase())).toBe(true);
      expect(check("conv-1")).toBe(true);
      expect(check("appr_1")).toBe(true);
    }
  });

  it.each(hostileIds)("rejects hostile id %j", (id) => {
    expect(isApiId(id)).toBe(false);
    expect(isConversationId(id)).toBe(false);
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
    expect(conversationApiUrl(uuid, "/cancel")).toBe(
      `/api/conversations/${uuid}/cancel`,
    );
    expect(conversationApiUrl(uuid, "/truncate?mode=edit_last")).toBe(
      `/api/conversations/${uuid}/truncate?mode=edit_last`,
    );
    expect(
      conversationApiUrl(uuid, `/stream?turn_id=${encodeURIComponent("t1")}`),
    ).toBe(
      `/api/conversations/${encodeURIComponent(uuid)}/stream?turn_id=${encodeURIComponent("t1")}`,
    );
  });

  it.each(hostileIds)("returns null for hostile id %j", (id) => {
    expect(conversationApiUrl(id)).toBeNull();
    expect(conversationApiUrl(id, "/rename")).toBeNull();
  });

  it("returns null for a traversal that tries to escape the prefix", () => {
    expect(conversationApiUrl(`${uuid}/../../login`)).toBeNull();
  });
});

describe("conversationApprovalApiUrl", () => {
  it("builds the approval path with both ids as encoded segments", () => {
    expect(conversationApprovalApiUrl(uuid, approvalUuid)).toBe(
      `/api/conversations/${uuid}/approvals/${approvalUuid}`,
    );
  });

  it.each(hostileIds)("returns null when the approval id is %j", (id) => {
    expect(conversationApprovalApiUrl(uuid, id)).toBeNull();
  });

  it.each(hostileIds)("returns null when the conversation id is %j", (id) => {
    expect(conversationApprovalApiUrl(id, approvalUuid)).toBeNull();
  });
});
