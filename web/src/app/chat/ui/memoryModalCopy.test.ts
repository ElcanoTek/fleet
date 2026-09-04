import { describe, expect, it } from "vitest";
import { memoryModalSubtitle } from "./memoryModalCopy";

// Finding #18: the modal's subtitle stayed on the personal-memory sentence
// whichever tab was showing, so on Team learnings neither half of it was true.

describe("memoryModalSubtitle", () => {
  const ctx = { userEmail: "sam@elcanotek.com", projectName: "test 2" };

  it("names the account and future chats on My memory", () => {
    expect(memoryModalSubtitle("list", ctx)).toBe(
      "Saved memories are scoped to sam@elcanotek.com and are added to future chats.",
    );
  });

  it("names the project and its chats on Team learnings", () => {
    const copy = memoryModalSubtitle("team", ctx);
    expect(copy).toBe(
      "Team learnings are shared with everyone in test 2 and added to every chat in it.",
    );
    // The two false claims the old subtitle made on this tab.
    expect(copy).not.toContain("sam@elcanotek.com");
    expect(copy).not.toContain("future chats");
  });

  it("describes derived entities and relations on Graph, not records", () => {
    const copy = memoryModalSubtitle("graph", ctx);
    expect(copy).toMatch(/entities and relations/i);
    expect(copy).toMatch(/derived/i);
    expect(copy).not.toContain("future chats");
  });

  it("falls back to the personal sentence when the team tab has no project", () => {
    // The team tab only exists inside a project; when the project is gone the
    // personal list is what renders, so the personal sentence is what is true.
    expect(memoryModalSubtitle("team", { userEmail: "sam@x.com" })).toBe(
      "Saved memories are scoped to sam@x.com and are added to future chats.",
    );
  });

  it("says 'this user' rather than 'undefined' before the session loads", () => {
    expect(memoryModalSubtitle("list", {})).toContain("scoped to this user");
  });
});
