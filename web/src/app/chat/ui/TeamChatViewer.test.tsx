import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { TeamChatViewer } from "./TeamChatViewer";

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

// JSX wraps the explainer across source lines; the DOM collapses that to
// single spaces, so match on normalized text.
const squash = (s: string) => s.replace(/\s+/g, " ").trim();

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
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

describe("TeamChatViewer — the Branch CTA (#10)", () => {
  it("keeps the button and its explainer exactly as they were", async () => {
    renderViewer();

    const button = await screen.findByRole("button", {
      name: "Branch to continue in your own chat",
    });
    expect(button).toBeEnabled();
    expect(
      screen.getByText((_, node) =>
        squash(node?.textContent ?? "") ===
        "You get your own copy in this project — private until you share it. sam’s chat is unchanged.",
      ),
    ).toBeInTheDocument();
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

  it("leaves the button an opaque control with a soft shadow", async () => {
    renderViewer();

    const button = await screen.findByRole("button", {
      name: "Branch to continue in your own chat",
    });
    // Opaque so conversation content never shows through the control itself,
    // shadowed so it reads as sitting above the page.
    expect(button.className).toContain("bg-[var(--color-surface-1)]");
    expect(button.className).toContain("shadow-[var(--shadow-md)]");
    // Unchanged from the shipped control: full width, same padding, same radius.
    expect(button.className).toContain("w-full");
    expect(button.className).toContain("px-4");
    expect(button.className).toContain("py-3");
    expect(button.className).toContain("rounded-[var(--radius-lg)]");
  });

  it("keeps the explainer above the fade so it stays legible over the transcript", async () => {
    renderViewer();

    await screen.findByRole("button", { name: "Branch to continue in your own chat" });
    const cta = screen.getByTestId("team-branch-cta");
    // The fade is absolutely positioned, so the copy over it must be
    // positioned too or it paints underneath.
    await waitFor(() => {
      const explainer = cta.querySelector("p");
      expect(explainer?.className).toContain("relative");
    });
  });
});
