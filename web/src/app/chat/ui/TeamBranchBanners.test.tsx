import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { BranchOriginBanner } from "./TeamBranchBanners";
import { WithheldFilesContext } from "./LockedFiles";
import { renderAssistantContent } from "./AssistantContent";
import type { BranchOrigin } from "./teamSharing";

// A teammate's branch (B20): where it came from, and — because only the files
// shared at branch time were copied — locked names for the rest.

afterEach(cleanup);

const ORIGIN: BranchOrigin = {
  source_conversation_id: "src-conv",
  source_owner_email: "sam@example.com",
  source_title: "Q3 CPA by domain",
  branched_at: 1759622400,
  copied_files: [{ path: "report.xlsx", name: "report.xlsx", size: 1 }],
  withheld_files: ["exclusion_list_v1.json"],
  source_still_shared: true,
};

describe("BranchOriginBanner", () => {
  it("says where the branch came from and links back while the original is shared", () => {
    const onOpenSource = vi.fn();
    render(<BranchOriginBanner origin={ORIGIN} onOpenSource={onOpenSource} />);
    expect(screen.getByTestId("branch-origin-banner")).toHaveTextContent(
      /Branched from sam@example\.com’s chat on Oct \d+\. Shared files came with it as your own copies\./,
    );
    fireEvent.click(screen.getByRole("button", { name: "Open Sam’s chat" }));
    expect(onOpenSource).toHaveBeenCalledWith("src-conv");
  });

  it("does not promise files when none came with the branch", () => {
    render(
      <BranchOriginBanner
        origin={{ ...ORIGIN, copied_files: [] }}
        onOpenSource={() => {}}
      />,
    );
    const banner = screen.getByTestId("branch-origin-banner");
    expect(banner).toHaveTextContent(/No files came with it\./);
    expect(banner).not.toHaveTextContent(/Shared files came with it/);
  });

  it("drops the link once the original is no longer shared", () => {
    render(
      <BranchOriginBanner
        origin={{ ...ORIGIN, source_still_shared: false }}
        onOpenSource={() => {}}
      />,
    );
    expect(screen.queryByRole("button")).toBeNull();
  });
});

describe("the branch transcript locks withheld files", () => {
  const CONV = "0f8fad5b-d9cb-469f-a165-70867728950e";
  const md = "Kept: [report.xlsx](report.xlsx). Left behind: [v1](exclusion_list_v1.json) ![c](exclusion_list_v1.json)";

  it("renders a withheld output as a locked name and keeps copied ones live", () => {
    const { container } = render(
      <WithheldFilesContext.Provider
        value={{ conversationId: CONV, withheld: new Set(["exclusion_list_v1.json"]) }}
      >
        {renderAssistantContent(md, false, CONV)}
      </WithheldFilesContext.Provider>,
    );
    expect(screen.getByRole("link", { name: "report.xlsx" })).toHaveAttribute(
      "href",
      `/api/conversations/${CONV}/workspace/report.xlsx`,
    );
    const locked = screen.getAllByTestId("locked-file");
    expect(locked).toHaveLength(2);
    for (const l of locked) expect(l).toHaveTextContent("exclusion_list_v1.json (not shared)");
    expect(container.querySelector('a[href*="exclusion_list"]')).toBeNull();
    expect(container.querySelector("img")).toBeNull();
  });

  it("with a truncated withheld list, only files the branch has stay live", () => {
    // `unknown.csv` is in neither list — past the server's bound — so it may
    // be a file the branch never received: locked, not a link that 404s.
    // `made-here.csv` is one of the branch's own outputs: live.
    const { container } = render(
      <WithheldFilesContext.Provider
        value={{
          conversationId: CONV,
          withheld: new Set(["exclusion_list_v1.json"]),
          available: new Set(["report.xlsx", "made-here.csv"]),
        }}
      >
        {renderAssistantContent(
          md + " Unknown: [u](unknown.csv). Mine: [m](made-here.csv)",
          false,
          CONV,
        )}
      </WithheldFilesContext.Provider>,
    );
    expect(screen.getByRole("link", { name: "report.xlsx" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "m" })).toHaveAttribute(
      "href",
      `/api/conversations/${CONV}/workspace/made-here.csv`,
    );
    expect(container.querySelector('a[href*="unknown.csv"]')).toBeNull();
    expect(
      screen.getAllByTestId("locked-file").some((l) => l.textContent === "unknown.csv (not shared)"),
    ).toBe(true);
  });

  it("lets the branch's own current outputs override the withheld list", () => {
    // The branch later created and presented its own exclusion_list_v1.json:
    // it is the branch's file now, so it is live, not locked forever — in the
    // complete-list case and in the truncated (allow-list) case alike.
    for (const available of [null, new Set(["report.xlsx"])]) {
      const { container, unmount } = render(
        <WithheldFilesContext.Provider
          value={{
            conversationId: CONV,
            withheld: new Set(["exclusion_list_v1.json"]),
            available,
            outputs: new Set(["exclusion_list_v1.json"]),
          }}
        >
          {renderAssistantContent(md, false, CONV)}
        </WithheldFilesContext.Provider>,
      );
      expect(screen.getByRole("link", { name: "v1" })).toHaveAttribute(
        "href",
        `/api/conversations/${CONV}/workspace/exclusion_list_v1.json`,
      );
      expect(container.querySelector("img")).not.toBeNull();
      expect(screen.queryByTestId("locked-file")).toBeNull();
      unmount();
    }
  });

  it("changes nothing without the context (every other chat)", () => {
    const { container } = render(<>{renderAssistantContent(md, false, CONV)}</>);
    expect(container.querySelector('a[href*="exclusion_list_v1.json"]')).not.toBeNull();
    expect(screen.queryByTestId("locked-file")).toBeNull();
  });
});
