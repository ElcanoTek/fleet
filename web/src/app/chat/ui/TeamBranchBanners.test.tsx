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

  it("changes nothing without the context (every other chat)", () => {
    const { container } = render(<>{renderAssistantContent(md, false, CONV)}</>);
    expect(container.querySelector('a[href*="exclusion_list_v1.json"]')).not.toBeNull();
    expect(screen.queryByTestId("locked-file")).toBeNull();
  });
});
