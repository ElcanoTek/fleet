import { afterEach, describe, expect, it } from "vitest";
import { bootTarget, claimHomeLanding, parseHome, type HomeConversation } from "./homeChat";

// The Home chat's boot rule and wire shape (docs/HOME-CHAT.md).

const HOME: HomeConversation = {
  conversation_id: "home-1",
  access: "owner",
  title: "Morning patrol",
  owner_email: "me@example.com",
};

const base = {
  deepLinkConvId: null,
  teamLink: null,
  newChatLink: false,
  home: HOME,
  landOnHome: () => true,
  mostRecentConvId: "recent-1",
};

describe("bootTarget", () => {
  it("lands on Home on a visit that lands there", () => {
    expect(bootTarget(base)).toEqual({ kind: "home", home: HOME });
  });

  it("falls back to the most recent chat when the visit does not land on Home", () => {
    expect(bootTarget({ ...base, landOnHome: () => false })).toEqual({ kind: "conversation", id: "recent-1" });
    expect(bootTarget({ ...base, home: null })).toEqual({ kind: "conversation", id: "recent-1" });
    expect(bootTarget({ ...base, home: null, mostRecentConvId: null })).toEqual({ kind: "none" });
  });

  it("lets every link win over Home, and does not use up the tab's landing", () => {
    let asked = 0;
    const landOnHome = () => {
      asked += 1;
      return true;
    };
    expect(bootTarget({ ...base, landOnHome, deepLinkConvId: "linked" })).toEqual({ kind: "conversation", id: "linked" });
    expect(bootTarget({ ...base, landOnHome, teamLink: { ownerConvId: "mine" } })).toEqual({ kind: "conversation", id: "mine" });
    // A teammate's team link shows the viewer or a notice; Home must not
    // replace it, so the most recent chat loads underneath as before.
    expect(bootTarget({ ...base, landOnHome, teamLink: { ownerConvId: null } })).toEqual({ kind: "conversation", id: "recent-1" });
    expect(bootTarget({ ...base, landOnHome, newChatLink: true })).toEqual({ kind: "none" });
    expect(asked).toBe(0);
  });
});

describe("parseHome", () => {
  it("reads the server's shape and rejects anything else", () => {
    expect(parseHome(HOME)).toEqual(HOME);
    expect(parseHome({ conversation_id: "x", access: "team_view" })).toEqual({
      conversation_id: "x",
      access: "team_view",
      title: "",
      owner_email: "",
    });
    for (const bad of [null, undefined, "x", {}, { conversation_id: "", access: "owner" }, { conversation_id: "x", access: "admin" }]) {
      expect(parseHome(bad)).toBeNull();
    }
  });
});

describe("claimHomeLanding", () => {
  afterEach(() => window.sessionStorage.clear());

  it("answers true once per tab", () => {
    expect(claimHomeLanding()).toBe(true);
    expect(claimHomeLanding()).toBe(false);
  });
});
