import { describe, expect, it } from "vitest";
import {
  conversationApiPath,
  conversationApiUrl,
  conversationApprovalApiUrl,
  conversationWorkspaceUrl,
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

describe("conversationApiPath", () => {
  it("encodes each literal and id segment on its own", () => {
    expect(conversationApiPath(uuid)).toBe(`/api/conversations/${uuid}`);
    expect(conversationApiPath(uuid, "queue", approvalUuid)).toBe(
      `/api/conversations/${uuid}/queue/${approvalUuid}`,
    );
    expect(conversationApiPath(uuid, "queue", approvalUuid, "send-now")).toBe(
      `/api/conversations/${uuid}/queue/${approvalUuid}/send-now`,
    );
    expect(conversationApiPath(uuid, "turns", approvalUuid)).toBe(
      `/api/conversations/${uuid}/turns/${approvalUuid}`,
    );
    expect(conversationApiPath(uuid, "subagents", "child_1")).toBe(
      `/api/conversations/${uuid}/subagents/child_1`,
    );
  });

  it.each(hostileIds)("returns null when a nested segment is %j", (id) => {
    expect(conversationApiPath(uuid, "queue", id)).toBeNull();
    expect(conversationApiPath(uuid, "queue", id, "send-now")).toBeNull();
    expect(conversationApiPath(uuid, "turns", id)).toBeNull();
  });

  it.each(hostileIds)("returns null when the conversation id is %j", (id) => {
    expect(conversationApiPath(id, "turns", approvalUuid)).toBeNull();
  });
});

describe("conversationWorkspaceUrl", () => {
  it("returns the workspace base for an empty path", () => {
    expect(conversationWorkspaceUrl(uuid)).toBe(
      `/api/conversations/${uuid}/workspace/`,
    );
  });

  it("allows dotted file names and nested dirs, encoding each segment", () => {
    expect(conversationWorkspaceUrl(uuid, "report.pptx")).toBe(
      `/api/conversations/${uuid}/workspace/report.pptx`,
    );
    expect(conversationWorkspaceUrl(uuid, "out/chart 1.png")).toBe(
      `/api/conversations/${uuid}/workspace/out/chart%201.png`,
    );
    // Encoded, not decoded: a literal `%2e%2e` or `?q` stays inside its segment.
    expect(conversationWorkspaceUrl(uuid, "%2e%2e/x")).toBe(
      `/api/conversations/${uuid}/workspace/%252e%252e/x`,
    );
    expect(conversationWorkspaceUrl(uuid, "a?q#h")).toBe(
      `/api/conversations/${uuid}/workspace/a%3Fq%23h`,
    );
    expect(conversationWorkspaceUrl(uuid, "http:")).toBe(
      `/api/conversations/${uuid}/workspace/http%3A`,
    );
  });

  it.each(["../x", "a/../b", "./x", "a//b", "/abs", "x/", "..", "."])(
    "refuses the workspace path %j",
    (path) => {
      expect(conversationWorkspaceUrl(uuid, path)).toBeNull();
    },
  );

  it.each(hostileIds)("returns null when the conversation id is %j", (id) => {
    expect(conversationWorkspaceUrl(id)).toBeNull();
    expect(conversationWorkspaceUrl(id, "report.pptx")).toBeNull();
  });
});
