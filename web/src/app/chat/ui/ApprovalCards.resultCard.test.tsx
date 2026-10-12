import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { ApprovalCard } from "./ApprovalCards";
import { retryFailedPrompt } from "./ApprovalResultCard";
import { approvalFromPoll } from "./useApprovalProgressPoll";
import {
  hydrateResolvedApproval,
  parseApprovalResultCardData,
  resultCardFromOutcome,
  type Approval,
  type ApprovalCardData,
} from "./history";

// A tool the bundle lists in critical_tool_result_cards returns an outcome
// card next to its result (docs/APPROVAL-RESULT-CARDS.md). The settled card
// then reads it: a one-line outcome in its tone, a row per record with both
// ids, a status and a link, plain failure wording, "Retry failed (N)", and the
// approved card's records, the raw call and the raw result under Details.
// Without a result card every settled card renders exactly as before.

const pendingCard: ApprovalCardData = {
  title: "Create 1 Index Exchange deal for Elcano",
  items: [{ label: "Elcano_ChatTest_IX_BidSwitch_Display_US", settings: [{ label: "Floor", value: "$1.00" }] }],
};

const created = {
  outcome: "done",
  title: "Created 1 Index Exchange deal · verified",
  items: [
    {
      key: "Elcano_ChatTest_IX_BidSwitch_Display_US",
      label: "Elcano_ChatTest_IX_BidSwitch_Display_US",
      id: "IX151710246087067804",
      internal_id: "666624",
      link: "https://app.example.com/deals/666624",
      link_label: "Open in Index Exchange",
      status: "done",
      note: "Index Exchange confirmed it after the create.",
    },
  ],
};

const plan = {
  outcome: "partial",
  title: "1 of 3 deals created · 1 failed",
  items: [
    { key: "A", label: "Deal A", id: "IX1", status: "done" },
    { key: "B", label: "Deal B", status: "failed", note: "the floor is below the SSP's minimum. Nothing was created." },
    { key: "C", label: "Deal C", status: "unclear", note: "Checking whether this deal was created." },
  ],
  retry: { keys: ["B"] },
};

const RESULT_TEXT = '{"success": true, "results": [{"deal_id": "A"}]}';

function renderCard(patch: Partial<Approval>, onRetryFailed = vi.fn()) {
  const full: Approval = {
    id: "ap_1",
    tool: "mcp_indexexchange_mcp_ix_execute_plan",
    status: "approved",
    resultText: RESULT_TEXT,
    summary: { tool: "mcp_indexexchange_mcp_ix_execute_plan", args: [{ key: "plan_id", value: "dp_1" }] },
    ...patch,
  } as Approval;
  render(<ApprovalCard approval={full} conversationId="conv_1" onResolved={vi.fn()} onRetryFailed={onRetryFailed} />);
  return { full, onRetryFailed };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("the completed card from a result card", () => {
  it("reads the outcome, the record with both ids, its status and link; raw JSON under Details", () => {
    renderCard({ card: pendingCard, resultCard: parseApprovalResultCardData(created) });
    const el = screen.getByTestId("generic-action-card");
    expect(el).toHaveAttribute("data-result-outcome", "done");
    expect(screen.getByTestId("generic-action-title")).toHaveTextContent(/^Created 1 Index Exchange deal · verified$/);
    // The approved card's title stays in view.
    expect(screen.getByTestId("approval-result-approved")).toHaveTextContent("Approved: Create 1 Index Exchange deal for Elcano");
    const item = screen.getByTestId("approval-result-item");
    expect(within(item).getByText("Elcano_ChatTest_IX_BidSwitch_Display_US")).toBeTruthy();
    expect(within(item).getByTestId("approval-result-id")).toHaveTextContent("IX151710246087067804");
    expect(within(item).getByTestId("approval-result-internal-id")).toHaveTextContent("internal 666624");
    expect(within(item).getByTestId("approval-result-status")).toHaveTextContent("Done");
    expect(within(item).getByRole("link", { name: /Open in Index Exchange/ })).toHaveAttribute("href", "https://app.example.com/deals/666624");
    expect(within(item).getByTestId("approval-result-note")).toHaveTextContent("Index Exchange confirmed it after the create.");
    // The raw result, the call and the approved records are behind Details.
    const details = screen.getByTestId("approval-card-details");
    expect(details).not.toBeVisible();
    fireEvent.click(screen.getByTestId("approval-card-details-toggle"));
    expect(details).toBeVisible();
    expect(within(details).getByTestId("approval-result")).toHaveTextContent('"deal_id": "A"');
    expect(within(details).getByText("mcp_indexexchange_mcp_ix_execute_plan")).toBeTruthy();
    expect(within(details).getByText("Floor:")).toBeTruthy();
    // Nothing to retry on a clean run.
    expect(screen.queryByTestId("approval-retry-failed")).toBeNull();
  });

  it("names failures plainly and offers Retry failed (N), which hands the keys to the caller once", () => {
    const { full, onRetryFailed } = renderCard({ card: pendingCard, resultCard: parseApprovalResultCardData(plan) });
    expect(screen.getByTestId("generic-action-card")).toHaveAttribute("data-result-outcome", "partial");
    const rows = screen.getAllByTestId("approval-result-item");
    expect(rows.map((r) => r.getAttribute("data-status"))).toEqual(["done", "failed", "unclear"]);
    expect(within(rows[1]).getByTestId("approval-result-status")).toHaveTextContent("Not applied");
    expect(within(rows[1]).getByTestId("approval-result-note")).toHaveTextContent(
      "Not applied: the floor is below the SSP's minimum. Nothing was created.",
    );
    expect(within(rows[2]).getByTestId("approval-result-status")).toHaveTextContent("Outcome unclear");
    expect(within(rows[2]).getByTestId("approval-result-note")).toHaveTextContent("Checking whether this deal was created.");
    const retry = screen.getByTestId("approval-retry-failed");
    expect(retry).toHaveTextContent("Retry failed (1)");
    fireEvent.click(retry);
    expect(onRetryFailed).toHaveBeenCalledTimes(1);
    expect(onRetryFailed).toHaveBeenCalledWith(expect.objectContaining({ id: full.id }), ["B"]);
    expect(retry).toBeDisabled();
    fireEvent.click(retry);
    expect(onRetryFailed).toHaveBeenCalledTimes(1);
  });

  it("says the read-back didn't match on a mismatch, in the warning tone", () => {
    renderCard({
      resultCard: parseApprovalResultCardData({
        outcome: "mismatch",
        title: "Sent · check didn't match",
        items: [{ label: "Deal A", id: "IX1", status: "mismatch", note: "Check the deal before retrying.", changes: [{ label: "Floor", before: "$55.00", after: "$0.10" }] }],
      }),
    });
    expect(screen.getByTestId("generic-action-card")).toHaveAttribute("data-result-outcome", "mismatch");
    expect(screen.getByTestId("approval-result-note")).toHaveTextContent(
      "Applied, but the read-back didn't match: Check the deal before retrying.",
    );
    expect(screen.getByTestId("approval-card-change")).toHaveTextContent("Floor$55.00→$0.10");
  });

  it("shows each record's flags on a several-record card too", () => {
    renderCard({
      resultCard: parseApprovalResultCardData({
        outcome: "done",
        title: "2 of 2 updated",
        items: [
          { label: "Deal A", status: "done", flags: [{ code: "deal_active", label: "Deal is Active" }] },
          { label: "Deal B", status: "done" },
        ],
      }),
    });
    const rows = screen.getAllByTestId("approval-result-item");
    expect(within(rows[0]).getByTestId("approval-card-flag")).toHaveTextContent("Deal is Active");
    expect(within(rows[1]).queryByTestId("approval-card-flag")).toBeNull();
  });

  it("folds the done records of a long plan into one row, failures stay in view", () => {
    const items = Array.from({ length: 12 }, (_, i) => ({ key: `k${i}`, label: `Deal ${i}`, status: i === 4 ? "failed" : "done" }));
    renderCard({ resultCard: parseApprovalResultCardData({ outcome: "partial", title: "11 of 12 created", items, retry: { keys: ["k4"] } }) });
    expect(screen.getByTestId("approval-result-done-summary")).toHaveTextContent("11 records done");
    expect(screen.getAllByTestId("approval-result-item")).toHaveLength(1);
    fireEvent.click(screen.getByTestId("approval-result-view-all"));
    expect(screen.getAllByTestId("approval-result-item")).toHaveLength(12);
  });

  it("a notify record says it ran without asking and keeps its record text", () => {
    renderCard({
      tool: "mcp_deal_sheet_next_campaign_id",
      recorded: true,
      resultText: "Ran without asking: this tool is declared notify-mode in the client bundle. Nothing to undo.",
      summary: { tool: "mcp_deal_sheet_next_campaign_id", args: [{ key: "idempotency_key", value: "booking:93615" }] },
      resultCard: parseApprovalResultCardData({ outcome: "done", title: "Campaign ID ELC07375 allocated", items: [] }),
    });
    expect(screen.getByTestId("generic-action-title")).toHaveTextContent(/^Campaign ID ELC07375 allocated · ran without asking$/);
    expect(screen.getByTestId("approval-record-note")).toHaveTextContent("Nothing to undo.");
    // The raw idempotency key is behind Details, not in the card's face.
    const details = screen.getByTestId("approval-card-details");
    expect(details).not.toBeVisible();
    expect(within(details).getByText("booking:93615")).toBeTruthy();
  });

  it.each([
    ["no result card", {}],
    ["a failed run", { status: "failed" as const }],
    ["an unknown outcome", { status: "execution_unknown" as const }],
    ["a decline", { status: "rejected" as const, resultText: "User declined this action." }],
  ])("keeps today's card for %s", (_, patch) => {
    const resultCard = "status" in patch ? parseApprovalResultCardData(created) : undefined;
    renderCard({ card: pendingCard, ...patch, resultCard });
    expect(screen.queryByTestId("approval-result-card")).toBeNull();
    expect(screen.getByTestId("generic-action-title").textContent).toMatch(/· Create 1 Index Exchange deal for Elcano$/);
  });
});

describe("parseApprovalResultCardData", () => {
  it("accepts the schema", () => {
    const c = parseApprovalResultCardData(plan);
    expect(c?.outcome).toBe("partial");
    expect(c?.retryKeys).toEqual(["B"]);
    expect(parseApprovalResultCardData(created)?.items[0]).toMatchObject({ internalId: "666624", linkLabel: "Open in Index Exchange" });
  });

  it("refuses anything that would make the card claim more than its rows", () => {
    for (const bad of [
      undefined,
      null,
      [],
      { title: "t", items: [] },
      { outcome: "great", title: "t", items: [] },
      { outcome: "done", title: "", items: [] },
      { outcome: "done", title: "t", items: [{ label: "A", status: "created" }] },
      { outcome: "done", title: "t", items: [{ label: "A", status: "failed" }] },
      { outcome: "done", title: "t", items: [{ label: "A", status: "unclear" }] },
      { outcome: "failed", title: "t", items: [{ label: "A", status: "done" }] },
      { outcome: "done", title: "t", items: [{ label: "A", status: "done", link: "http://x.example.com" }] },
      { outcome: "done", title: "t", items: [{ label: "A", status: "done", link_label: "Open" }] },
      { outcome: "done", title: "t", items: [{ key: "A", label: "A", status: "done" }, { key: "A", label: "B", status: "done" }] },
      { outcome: "partial", title: "t", items: [{ key: "B", label: "B", status: "failed" }], retry: { keys: [] } },
      { outcome: "partial", title: "t", items: [{ key: "B", label: "B", status: "failed" }], retry: { keys: ["Z"] } },
      { outcome: "unclear", title: "t", items: [{ key: "U", label: "U", status: "unclear" }], retry: { keys: ["U"] } },
      { outcome: "mismatch", title: "t", items: [{ key: "M", label: "M", status: "mismatch" }], retry: { keys: ["M"] } },
    ]) {
      expect(parseApprovalResultCardData(bad), JSON.stringify(bad)).toBeUndefined();
    }
  });

  it("reads a result card only from a recorded success", () => {
    expect(resultCardFromOutcome({ status: "approved", is_err: false, result_card: created })).toBeDefined();
    expect(resultCardFromOutcome({ status: "approved", is_err: true, result_card: created })).toBeUndefined();
    expect(resultCardFromOutcome({ status: "approved", result_card: created })).toBeUndefined();
    expect(resultCardFromOutcome({ status: "approved", executing: true, result_card: created })).toBeUndefined();
    expect(resultCardFromOutcome({ status: "rejected", result_card: created })).toBeUndefined();
    const a = hydrateResolvedApproval({
      approval_id: "ap_1",
      tool: "mcp_x_execute_plan",
      summary: {},
      status: "approved",
      is_err: false,
      result_text: "{}",
      result_card: created,
    });
    expect(a.resultCard?.title).toBe("Created 1 Index Exchange deal · verified");
  });
});

describe("retryFailedPrompt", () => {
  it("names the approved call and the keys, and asks for a new approval", () => {
    const text = retryFailedPrompt({ id: "ap_9", tool: "mcp_ix_execute_plan" } as Approval, ["Deal B", 'Deal "C"']);
    expect(text).toBe(
      'Retry the 2 failed items from the approved mcp_ix_execute_plan call (approval_id=ap_9): "Deal B", "Deal \\"C\\"". Retry only these, and stage the retry for my approval.',
    );
  });
});

describe("the answer that settles a card carries its result card", () => {
  it("Approve maps the POST answer's result_card onto the settled card", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ status: "approved", is_err: false, result_text: RESULT_TEXT, result_card: created }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const onResolved = vi.fn();
    const pending: Approval = {
      id: "ap_1",
      tool: "mcp_indexexchange_mcp_ix_execute_plan",
      status: "pending",
      card: pendingCard,
      summary: { tool: "mcp_indexexchange_mcp_ix_execute_plan", args: [] },
    };
    render(<ApprovalCard approval={pending} conversationId="conv_1" onResolved={onResolved} />);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    });
    expect(onResolved).toHaveBeenLastCalledWith(
      expect.objectContaining({ status: "approved", resultCard: expect.objectContaining({ outcome: "done" }) }),
    );
  });

  it("a failed POST answer carries no result card even if one is sent", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ status: "approved", is_err: true, result_text: "boom", result_card: created }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const onResolved = vi.fn();
    const pending: Approval = { id: "ap_1", tool: "mcp_x_execute_plan", status: "pending", card: pendingCard, summary: { args: [] } };
    render(<ApprovalCard approval={pending} conversationId="conv_1" onResolved={onResolved} />);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    });
    expect(onResolved).toHaveBeenLastCalledWith(expect.objectContaining({ status: "failed", resultCard: undefined }));
  });

  it("the progress poll settles a running card with its result card", () => {
    const cur: Approval = { id: "ap-run", tool: "mcp_x_execute_plan", status: "pending", executing: true, summary: {} };
    const next = approvalFromPoll(cur, {
      resolved_approvals: [{ approval_id: "ap-run", status: "approved", is_err: false, result_text: "{}", result_card: created }],
    });
    expect(next?.resultCard?.outcome).toBe("done");
  });
});
