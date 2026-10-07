import { afterEach, describe, expect, it, vi } from "vitest";
import { shareChatWithTeam } from "./teamSharing";

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
