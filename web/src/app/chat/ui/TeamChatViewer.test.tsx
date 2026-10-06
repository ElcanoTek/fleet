import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ChatToastProvider } from "./ChatToasts";
import { TEAM_VIEW_POLL_MS, TeamChatViewer } from "./TeamChatViewer";

// What a teammate actually meets when they open a chat someone shared with the
// team (Item C4, ADR-0057). Two guarantees are covered here, both reported from
// QA against the shipped view:
//
//   #9  the transcript names files it does not hand over, so nothing in it may
//       render as a live link or a broken image;
//   #10 the Branch CTA sits where the composer would be, over a scrolling
//       transcript, so it needs the composer's own legibility treatment rather
//       than a flat panel plated across the conversation.

const OWNER = "sam@example.com";

const SNAPSHOT = {
  id: "conv-1",
  owner_email: OWNER,
  title: "Channel spend review",
  team_id: "growth",
  updated_at: 1767225600,
  messages: [
    { id: 1, role: "user", type: "text", content: { text: "Break spend down by channel." } },
    {
      id: 2,
      role: "assistant",
      type: "text",
      content: {
        text: [
          "Here is the breakdown.",
          "",
          "![Daily spend by channel](daily_spend_by_channel.png)",
          "",
          "Full data: [daily_spend_by_channel.csv](daily_spend_by_channel.csv)",
          "",
          "Method: [the attribution docs](https://example.com/attribution).",
        ].join("\n"),
      },
    },
  ],
};

function mockTeamView() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/team-view")) {
        return new Response(JSON.stringify(SNAPSHOT), { status: 200 });
      }
      return new Response(JSON.stringify({}), { status: 200 });
    }),
  );
}

function renderViewer() {
  mockTeamView();
  return render(
    <TeamChatViewer conversationId="conv-1" onBack={() => {}} onBranched={() => {}} />,
  );
}


afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("TeamChatViewer — the read-only transcript withholds files (#9)", () => {
  it("names a workspace file but links nothing", async () => {
    const { container } = renderViewer();

    expect(
      await screen.findByText(/daily_spend_by_channel\.csv \(file not shared\)/),
    ).toBeInTheDocument();
    expect(container.querySelector('a[href*="daily_spend_by_channel"]')).toBeNull();
  });

  it("says the image was not shared, rather than that loading it failed", async () => {
    const { container } = renderViewer();

    expect(
      await screen.findByText("Image not shared with team views."),
    ).toBeInTheDocument();
    expect(container.querySelector("img")).toBeNull();
    expect(screen.queryByText(/couldn’t load image/i)).toBeNull();
  });

  it("still lets a teammate follow an ordinary external link", async () => {
    renderViewer();

    const link = await screen.findByRole("link", { name: "the attribution docs" });
    expect(link).toHaveAttribute("href", "https://example.com/attribution");
    expect(screen.getAllByRole("link")).toHaveLength(1);
  });
});

describe("TeamChatViewer — the Branch CTA (#10, B19)", () => {
  it("says whose chat this is where the composer would be, with the one action", async () => {
    renderViewer();

    const button = await screen.findByRole("button", {
      name: "Branch to continue in your own chat",
    });
    expect(button).toBeEnabled();
    expect(screen.getByText("Read-only. This is Sam’s chat.")).toBeInTheDocument();
  });

  it("fades into the page background instead of plating a panel over the transcript", async () => {
    renderViewer();

    await screen.findByRole("button", { name: "Branch to continue in your own chat" });
    const cta = screen.getByTestId("team-branch-cta");

    // The composer's own treatment: one soft gradient from the page
    // background, reaching above the CTA region, drawn behind everything and
    // inert to the pointer. --sticky-fade is theme-swapped in globals.css, so
    // light and dark each fade to their own --color-bg.
    const fade = cta.querySelector("[aria-hidden='true']");
    expect(fade).not.toBeNull();
    expect(fade?.className).toContain("bg-[image:var(--sticky-fade)]");
    expect(fade?.className).toContain("pointer-events-none");
    expect(fade?.className).toContain("-top-16");

    // No box edges and no opaque plate on the region itself — that is what
    // read as unfinished.
    expect(cta.className).not.toContain("border-t");
    expect(cta.className).not.toContain("bg-[var(--color-surface-1)]");
  });

  it("sits the line and button in an opaque, positioned bar above the fade", async () => {
    renderViewer();

    const button = await screen.findByRole("button", {
      name: "Branch to continue in your own chat",
    });
    // The primary action: the brand fill with its purpose-built foreground.
    expect(button.className).toContain("bg-[var(--color-primary)]");
    expect(button.className).toContain("text-[var(--color-on-primary)]");
    // The bar is opaque so conversation content never shows through, and
    // positioned so it paints over the absolutely-positioned fade.
    const bar = button.parentElement!;
    expect(bar.className).toContain("bg-[var(--color-surface-1)]");
    expect(bar.className).toContain("relative");
    expect(bar.className).toContain("shadow-[var(--shadow-md)]");
  });
});

// ── B19–B21 with the extended team-view (files, project, viewer_branch) ────

const CONV = "0f8fad5b-d9cb-469f-a165-70867728950e";

const WITH_FILES = {
  ...SNAPSHOT,
  id: CONV,
  project_id: "proj-1",
  project_name: "Knowertech",
  files: [
    { path: "daily_spend_by_channel.png", name: "daily_spend_by_channel.png", size: 10, modified_at: 1, shared: true },
    { path: "daily_spend_by_channel.csv", name: "daily_spend_by_channel.csv", size: 10, modified_at: 1, shared: false },
  ],
  viewer_branch: null as null | { conversation_id: string; branched_at: number; changed_since: boolean },
};

type Snap = typeof WITH_FILES;

function stubFetch(handler: (url: string, init?: RequestInit) => Response | Promise<Response>) {
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => handler(String(input), init));
  vi.stubGlobal("fetch", fn);
  return fn;
}

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status });
}

function renderFull(
  snap: Snap,
  props: Partial<React.ComponentProps<typeof TeamChatViewer>> = {},
) {
  return render(
    <ChatToastProvider>
      <TeamChatViewer
        conversationId={CONV}
        onBack={() => {}}
        onBranched={() => {}}
        {...props}
      />
    </ChatToastProvider>,
  );
}

describe("TeamChatViewer — read-only view with shared files (B19)", () => {
  it("serves a shared output from the team-files route and locks an unshared one", async () => {
    stubFetch((url) => (url.includes("/team-view") ? json(WITH_FILES) : json({})));
    const { container } = renderFull(WITH_FILES);

    const locked = await screen.findByTestId("locked-file");
    expect(locked).toHaveTextContent("daily_spend_by_channel.csv (not shared)");
    expect(container.querySelector('a[href*="daily_spend_by_channel.csv"]')).toBeNull();
    await waitFor(() => {
      const img = container.querySelector("img");
      expect(img?.getAttribute("src")).toBe(
        `/api/conversations/${CONV}/team-files/daily_spend_by_channel.png`,
      );
    });
    // Nothing is ever requested from the owner's own workspace route.
    expect(container.innerHTML).not.toContain("/workspace/");
  });

  it("shows the project breadcrumb, the title and who shared it", async () => {
    stubFetch(() => json(WITH_FILES));
    const onOpenProject = vi.fn();
    renderFull(WITH_FILES, { onOpenProject });

    fireEvent.click(await screen.findByRole("button", { name: "Knowertech" }));
    expect(onOpenProject).toHaveBeenCalledWith("proj-1");
    expect(screen.getByRole("heading", { name: "Channel spend review" })).toBeInTheDocument();
    expect(screen.getByTestId("team-view-shared-by")).toHaveTextContent(`Shared by ${OWNER}`);
  });
});

describe("TeamChatViewer — branching (B20)", () => {
  it("creates '<chat> (branch)', confirms with the toast, and opens the branch", async () => {
    const fetchMock = stubFetch((url) => {
      if (url.includes("/team-view")) return json(WITH_FILES);
      if (url.includes("/branch"))
        return json(
          {
            id: "new-conv",
            branch_origin: {
              copied_files: [
                { path: "a.csv", name: "a.csv", size: 1 },
                { path: "b.csv", name: "b.csv", size: 1 },
              ],
            },
          },
          201,
        );
      return json({});
    });
    const onBranched = vi.fn();
    renderFull(WITH_FILES, { onBranched });

    fireEvent.click(
      await screen.findByRole("button", { name: "Branch to continue in your own chat" }),
    );
    await waitFor(() => expect(onBranched).toHaveBeenCalledWith("new-conv"));
    const branchCall = fetchMock.mock.calls.find(([u]) => String(u).includes("/branch"));
    expect(JSON.parse(String(branchCall?.[1]?.body))).toEqual({
      branch_point_message_id: 2,
      title: "Channel spend review (branch)",
    });
    expect(
      await screen.findByText("Branched into your own chat. 2 shared files came with it."),
    ).toBeInTheDocument();
  });

  it("says no files came along when the branch copied none", async () => {
    stubFetch((url) => {
      if (url.includes("/team-view")) return json(WITH_FILES);
      if (url.includes("/branch"))
        return json({ id: "new-conv", branch_origin: { copied_files: [] } }, 201);
      return json({});
    });
    const onBranched = vi.fn();
    renderFull(WITH_FILES, { onBranched });
    fireEvent.click(
      await screen.findByRole("button", { name: "Branch to continue in your own chat" }),
    );
    expect(
      await screen.findByText("Branched into your own chat. No files came with it."),
    ).toBeInTheDocument();
  });
});

describe("TeamChatViewer — coming back to a chat you branched (B21)", () => {
  const BRANCHED: Snap = {
    ...WITH_FILES,
    viewer_branch: { conversation_id: "my-branch", branched_at: 1759622400, changed_since: true },
  };

  it("links to the branch, says the owner has added messages, and offers Branch again", async () => {
    stubFetch(() => json(BRANCHED));
    const onOpenBranch = vi.fn();
    renderFull(BRANCHED, { onOpenBranch });

    const banner = await screen.findByTestId("viewer-branch-banner");
    expect(banner).toHaveTextContent(/You branched this on Oct \d+/);
    expect(banner).toHaveTextContent("Sam has added messages since you branched.");
    fireEvent.click(screen.getByRole("button", { name: "Open your branch" }));
    expect(onOpenBranch).toHaveBeenCalledWith("my-branch");
    expect(screen.getByRole("button", { name: "Branch again" })).toBeInTheDocument();
  });

  it("omits the 'added messages' line when nothing changed since", async () => {
    const same = { ...BRANCHED, viewer_branch: { ...BRANCHED.viewer_branch!, changed_since: false } };
    stubFetch(() => json(same));
    renderFull(same);
    const banner = await screen.findByTestId("viewer-branch-banner");
    expect(banner).not.toHaveTextContent("added messages");
  });
});

describe("TeamChatViewer — live (the owner keeps working)", () => {
  it("re-reads the chat on an interval and shows new messages", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    let snap: Snap = WITH_FILES;
    const fetchMock = stubFetch(() => json(snap));
    renderFull(WITH_FILES);
    await screen.findByText("Break spend down by channel.");

    snap = {
      ...WITH_FILES,
      messages: [
        ...WITH_FILES.messages,
        { id: 3, role: "user", type: "text", content: { text: "Now by region." } },
      ],
    };
    await act(async () => {
      vi.advanceTimersByTime(TEAM_VIEW_POLL_MS);
    });
    expect(await screen.findByText("Now by region.")).toBeInTheDocument();
    expect(fetchMock.mock.calls.length).toBeGreaterThanOrEqual(2);
  });

  it("drops the transcript when the chat stops being shared", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    let status = 200;
    stubFetch(() => (status === 200 ? json(WITH_FILES) : json({}, 404)));
    renderFull(WITH_FILES);
    await screen.findByText("Break spend down by channel.");

    status = 404;
    await act(async () => {
      vi.advanceTimersByTime(TEAM_VIEW_POLL_MS);
    });
    expect(
      await screen.findByText("This chat isn’t shared with your team anymore."),
    ).toBeInTheDocument();
    expect(screen.queryByText("Break spend down by channel.")).toBeNull();
  });

  it("clears a failed first load once a poll succeeds", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    let status = 500;
    stubFetch(() => (status === 200 ? json(WITH_FILES) : json({}, status)));
    renderFull(WITH_FILES);
    expect(await screen.findByText("Couldn’t load this chat (HTTP 500).")).toBeInTheDocument();

    status = 200;
    await act(async () => {
      vi.advanceTimersByTime(TEAM_VIEW_POLL_MS);
    });
    expect(await screen.findByText("Break spend down by channel.")).toBeInTheDocument();
    // Neither above the transcript nor beside the Branch button.
    expect(screen.queryByText(/Couldn’t load this chat/)).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
