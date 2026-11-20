import { afterEach, describe, expect, it, vi } from "vitest";
import {
  branchSourceRoute,
  shareChatWithTeam,
  teamLinkRoute,
  type BranchOrigin,
} from "./teamSharing";

// Where a teammate's chat opens is the server's call, read the same way at
// every door (docs/TEAM-SHARING.md): live for a Multiplayer participant,
// read-only for a teammate who may only read, nowhere for anyone else.
describe("teamLinkRoute", () => {
  const status = (s: string) => ({ status: s, viewer_email: "k@example.com" }) as never;
  it("routes each server answer", () => {
    expect(teamLinkRoute(status("owner"))).toBe("boot");
    expect(teamLinkRoute(status("multiplayer"))).toBe("live");
    expect(teamLinkRoute(status("open"))).toBe("read_only");
    expect(teamLinkRoute(status("not_on_team"))).toBe("notice");
    expect(teamLinkRoute(status("not_shared"))).toBe("notice");
  });
  it("sends a failed lookup to the viewer, which reports its own error", () => {
    expect(teamLinkRoute(null)).toBe("read_only");
  });
});

describe("branchSourceRoute", () => {
  const origin = (o: Partial<BranchOrigin>): BranchOrigin => ({
    source_conversation_id: "src",
    source_owner_email: "abbey@example.com",
    source_title: "Opti",
    branched_at: 1,
    copied_files: [],
    withheld_files: [],
    source_still_shared: false,
    ...o,
  });
  it("opens a source the brancher takes part in live, shared or not", () => {
    expect(branchSourceRoute(origin({ source_multiplayer: true, source_still_shared: true }))).toBe("live");
    expect(branchSourceRoute(origin({ source_multiplayer: true }))).toBe("live");
  });
  it("opens a source shared only with the team read-only", () => {
    expect(branchSourceRoute(origin({ source_still_shared: true }))).toBe("read_only");
    // An older server sends no source_multiplayer.
    expect(branchSourceRoute(origin({ source_still_shared: true, source_multiplayer: undefined }))).toBe("read_only");
  });
  it("offers no link once the brancher can open neither", () => {
    expect(branchSourceRoute(origin({}))).toBeNull();
  });
});

describe("shareChatWithTeam", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  const stubFetch = () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(
          JSON.stringify({ team_visible: true, shared_files: 1, total_files: 2 }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);
    return fetchMock;
  };
  const sentBody = (fetchMock: ReturnType<typeof stubFetch>) =>
    JSON.parse(String((fetchMock.mock.calls[0] as unknown as [string, RequestInit])[1].body));

  it("sends the checklist with the paths it LISTED, so unlisted exclusions stand", async () => {
    const fetchMock = stubFetch();
    await shareChatWithTeam("c1", true, {
      listedPaths: ["a.csv", "b.csv"],
      unsharedPaths: ["b.csv"],
    });
    expect(sentBody(fetchMock)).toEqual({
      visible: true,
      unshared_paths: ["b.csv"],
      listed_paths: ["a.csv", "b.csv"],
    });
  });

  it("sends no checklist for a one-click share or an unshare", async () => {
    let fetchMock = stubFetch();
    await shareChatWithTeam("c1", true);
    expect(sentBody(fetchMock)).toEqual({ visible: true });
    fetchMock = stubFetch();
    await shareChatWithTeam("c1", false, { listedPaths: ["a.csv"], unsharedPaths: [] });
    expect(sentBody(fetchMock)).toEqual({ visible: false });
  });
});
