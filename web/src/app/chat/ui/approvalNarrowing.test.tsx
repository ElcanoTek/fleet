import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ApprovalCard } from "./ApprovalCards";
import { ApprovalGroupCard } from "./ApprovalGroupCard";
import {
  hydrateResolvedApproval,
  parseApprovalCardData,
  parseApprovalNarrowable,
  parseApprovalNarrowed,
  type Approval,
  type ApprovalCardData,
} from "./history";
import { approvedValuesFor, narrowedApproveLabel, narrowedSummary, selectableKeys } from "./approvalNarrowing";
import { approvalFromPoll } from "./useApprovalProgressPoll";

// Approval narrowing (docs/APPROVAL-NARROWING.md): a readable card whose tool
// declares a narrowable list argument shows a checkbox on every row that names
// a staged value; the person unticks rows and approves only the rest, in one
// click ("Approve k of n"). Nothing ticked declines the action.

const card: ApprovalCardData = {
  title: "Update floor on 3 Index Exchange deals",
  subtitle: "Floor $0.10 → $5.00 · uncheck any deal to leave it out",
  items: [
    { key: "k1", label: "Deal one", id: "IX1", changes: [{ label: "Floor", before: "$0.10", after: "$5.00" }] },
    { key: "k2", label: "Deal two", id: "IX2", changes: [{ label: "Floor", before: "$0.10", after: "$5.00" }], flags: [{ code: "deal_active", label: "Deal is Active" }] },
    { key: "k3", label: "Deal three", id: "IX3", changes: [{ label: "Floor", before: "$0.10", after: "$5.00" }] },
  ],
  footer: "Approving runs only the 3 changes above",
};

function pending(overrides: Partial<Approval> = {}): Approval {
  return {
    id: "ap_1",
    tool: "mcp_ix_execute_plan",
    status: "pending",
    summary: { tool: "mcp_ix_execute_plan", args: [{ key: "deal_ids", value: '["k1","k2","k3"]' }] },
    card,
    narrowable: { arg: "deal_ids", values: ["k1", "k2", "k3"] },
    noSessionApproval: true,
    expiresAt: Math.floor(Date.now() / 1000) + 3600,
    ...overrides,
  };
}

function okResponse(body: Record<string, unknown>) {
  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("narrowing helpers", () => {
  it("parses the server's payloads strictly", () => {
    expect(parseApprovalNarrowable({ arg: "deal_ids", values: ["a", "b"] })).toEqual({ arg: "deal_ids", values: ["a", "b"] });
    for (const bad of [null, { arg: "deal_ids", values: ["a"] }, { arg: "", values: ["a", "b"] }, { arg: "x", values: ["a", "a"] }, { arg: "x", values: ["a", 1] }]) {
      expect(parseApprovalNarrowable(bad)).toBeUndefined();
    }
    expect(parseApprovalNarrowed({ arg: "deal_ids", staged: ["a", "b", "c"], approved: ["a"] })).toEqual({ arg: "deal_ids", staged: ["a", "b", "c"], approved: ["a"] });
    for (const bad of [
      { arg: "deal_ids", staged: ["a", "b"], approved: ["a", "b"] }, // not narrowed
      { arg: "deal_ids", staged: ["a", "b"], approved: ["z"] }, // not staged
      { arg: "deal_ids", staged: ["a", "b"], approved: [] },
    ]) {
      expect(parseApprovalNarrowed(bad)).toBeUndefined();
    }
  });

  it("keeps a row key on the card and refuses a repeated one", () => {
    expect(parseApprovalCardData({ title: "t", items: [{ key: "a", label: "A" }] })?.items[0].key).toBe("a");
    expect(parseApprovalCardData({ title: "t", items: [{ key: "a", label: "A" }, { key: "a", label: "B" }] })).toBeUndefined();
    expect(parseApprovalCardData({ title: "t", items: [{ key: 7, label: "A" }] })).toBeUndefined();
  });

  it("selects only rows that name a staged value, and keeps staged order", () => {
    const keys = selectableKeys({ narrowable: { arg: "deal_ids", values: ["k1", "k3"] }, card });
    expect([...keys]).toEqual(["k1", "k3"]);
    expect(selectableKeys({ card }).size).toBe(0);
    expect(approvedValuesFor({ arg: "d", values: ["a", "b", "c"] }, new Set(["b"]))).toEqual(["a", "c"]);
    expect(narrowedApproveLabel(3, 3)).toBe("Approve");
    expect(narrowedApproveLabel(2, 3)).toBe("Approve 2 of 3");
    expect(narrowedSummary({ arg: "d", staged: ["a", "b", "c"], approved: ["a"] })).toBe("Approved 1 of 3 · 2 left out");
  });

  it("hydrates a settled card's narrowed record", () => {
    const a = hydrateResolvedApproval({
      approval_id: "ap_1",
      tool: "mcp_ix_execute_plan",
      summary: {},
      status: "approved",
      is_err: false,
      narrowed: { arg: "deal_ids", staged: ["k1", "k2"], approved: ["k2"] },
    });
    expect(a.narrowed).toEqual({ arg: "deal_ids", staged: ["k1", "k2"], approved: ["k2"] });
  });
});

describe("a narrowable pending card", () => {
  it("unticks a row, shows it Left out, and approves the rest in one click", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      okResponse({ status: "approved", is_err: false, result_text: "ok", narrowed: { arg: "deal_ids", staged: ["k1", "k2", "k3"], approved: ["k1", "k3"] } }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const onResolved = vi.fn();
    render(<ApprovalCard approval={pending()} conversationId="conv_1" onResolved={onResolved} />);

    const checks = screen.getAllByTestId("approval-card-row-check");
    expect(checks).toHaveLength(3);
    expect(checks[1]).toHaveAccessibleName("Include Deal two (IX2)");
    expect(screen.getByTestId("approval-narrow-count")).toHaveTextContent("3 of 3 selected");
    expect(screen.getByTestId("approval-approve")).toHaveTextContent(/^Approve$/);

    fireEvent.click(checks[1]);
    const rows = screen.getAllByTestId("approval-card-item");
    expect(rows[1]).toHaveAttribute("data-left-out", "true");
    expect(within(rows[1]).getByTestId("approval-card-left-out")).toHaveTextContent("Left out");
    expect(screen.getByTestId("approval-narrow-count")).toHaveTextContent("2 of 3 selected");
    expect(screen.getByTestId("approval-approve")).toHaveTextContent("Approve 2 of 3");
    expect(screen.getByTestId("approval-narrow-scope")).toHaveTextContent("Covers only the 2 selected; 1 left out of the call.");

    fireEvent.click(screen.getByTestId("approval-approve"));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/conversations/conv_1/approvals/ap_1");
    expect(JSON.parse(String(init?.body))).toEqual({
      approved: true,
      scope: "once",
      approved_values: ["k1", "k3"],
      narrowed_arg: "deal_ids",
    });
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
    expect(onResolved.mock.calls[0][0].narrowed).toEqual({ arg: "deal_ids", staged: ["k1", "k2", "k3"], approved: ["k1", "k3"] });
  });

  it("approves the whole call when every row stays ticked", async () => {
    const fetchMock = vi.fn().mockResolvedValue(okResponse({ status: "approved", is_err: false, result_text: "ok" }));
    vi.stubGlobal("fetch", fetchMock);
    render(<ApprovalCard approval={pending()} conversationId="conv_1" onResolved={vi.fn()} />);
    fireEvent.click(screen.getByTestId("approval-approve"));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({ approved: true, scope: "once" });
  });

  it("declines when nothing is ticked", async () => {
    const fetchMock = vi.fn().mockResolvedValue(okResponse({ status: "rejected" }));
    vi.stubGlobal("fetch", fetchMock);
    render(<ApprovalCard approval={pending()} conversationId="conv_1" onResolved={vi.fn()} />);
    for (const c of screen.getAllByTestId("approval-card-row-check")) fireEvent.click(c);
    expect(screen.getByTestId("approval-approve")).toHaveTextContent("Decline");
    expect(screen.getByTestId("approval-narrow-scope")).toHaveTextContent("Nothing is selected, so this declines the action.");
    fireEvent.click(screen.getByTestId("approval-approve"));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({ approved: false, scope: "once" });
  });

  it("shows no checkbox without a narrowable payload, or for a row that names no staged value", () => {
    render(<ApprovalCard approval={pending({ narrowable: undefined })} conversationId="conv_1" onResolved={vi.fn()} />);
    expect(screen.queryAllByTestId("approval-card-row-check")).toHaveLength(0);
    expect(screen.queryByTestId("approval-narrow-count")).toBeNull();
    cleanup();
    render(<ApprovalCard approval={pending({ narrowable: { arg: "deal_ids", values: ["k1", "k3", "k9"] } })} conversationId="conv_1" onResolved={vi.fn()} />);
    expect(screen.getAllByTestId("approval-card-row-check")).toHaveLength(2);
  });

  it("a settled narrowed card says what was approved and marks the left-out rows", () => {
    render(
      <ApprovalCard
        approval={pending({
          status: "approved",
          resultText: "ok",
          narrowable: undefined,
          narrowed: { arg: "deal_ids", staged: ["k1", "k2", "k3"], approved: ["k1", "k3"] },
        })}
        conversationId="conv_1"
        onResolved={vi.fn()}
      />,
    );
    expect(screen.getByTestId("approval-narrowed-summary")).toHaveTextContent("Approved 2 of 3 · 1 left out");
    const left = screen.getAllByTestId("approval-card-item").filter((r) => r.getAttribute("data-left-out") === "true");
    expect(left).toHaveLength(1);
    expect(left[0]).toHaveTextContent("Deal two");
    expect(left[0]).toHaveTextContent("Left out · unchecked before approval");
  });
});

describe("narrowing inside a grouped card", () => {
  const groupId = "11111111-2222-3333-4444-555555555555";
  function members(): Approval[] {
    return [
      pending({ id: "ap-ix", groupId }),
      {
        id: "ap-mg",
        tool: "mcp_magnite_execute_plan",
        status: "pending",
        summary: { tool: "mcp_magnite_execute_plan", args: [{ key: "deal_ids", value: '["m1","m2"]' }] },
        groupId,
        expiresAt: Math.floor(Date.now() / 1000) + 600,
      },
    ];
  }

  it("names a call's ticked values under narrow, and declines a call left with none", async () => {
    const fetchMock = vi.fn().mockResolvedValue(okResponse({ results: [] }));
    vi.stubGlobal("fetch", fetchMock);
    render(<ApprovalGroupCard groupId={groupId} approvals={members()} conversationId="conv_1" onResolved={vi.fn()} onOneAtATime={vi.fn()} />);
    const checks = screen.getAllByTestId("approval-card-row-check");
    expect(checks).toHaveLength(3);
    fireEvent.click(checks[0]);
    expect(screen.getByTestId("approval-group-narrow-count")).toHaveTextContent("2 of 3 selected");
    fireEvent.click(screen.getByTestId("approval-group-approve"));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({
      approve: ["ap-ix", "ap-mg"],
      decline: [],
      narrow: { "ap-ix": ["k2", "k3"] },
      narrow_args: { "ap-ix": "deal_ids" },
    });

    cleanup();
    fetchMock.mockClear();
    render(<ApprovalGroupCard groupId={groupId} approvals={members()} conversationId="conv_1" onResolved={vi.fn()} onOneAtATime={vi.fn()} />);
    for (const c of screen.getAllByTestId("approval-card-row-check")) fireEvent.click(c);
    expect(screen.getByTestId("approval-group-approve")).toHaveTextContent("Approve all (1)");
    fireEvent.click(screen.getByTestId("approval-group-approve"));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({ approve: ["ap-mg"], decline: ["ap-ix"] });
  });
});

describe("a narrowed call that settles by the poll", () => {
  it("keeps what was approved when the polled row carries it", () => {
    const current = pending({ status: "pending", executing: true, narrowable: undefined });
    const settled = approvalFromPoll(current, {
      resolved_approvals: [
        { approval_id: "ap_1", status: "approved", is_err: false, narrowed: { arg: "deal_ids", staged: ["k1", "k2", "k3"], approved: ["k1"] } },
      ],
    });
    expect(settled?.narrowed).toEqual({ arg: "deal_ids", staged: ["k1", "k2", "k3"], approved: ["k1"] });
  });
});

describe("a narrowed approval without a readable card", () => {
  it("still says what was approved on the settled generic card", () => {
    render(
      <ApprovalCard
        approval={pending({
          card: undefined,
          narrowable: undefined,
          status: "approved",
          resultText: "ok",
          narrowed: { arg: "deal_ids", staged: ["k1", "k2", "k3"], approved: ["k1"] },
        })}
        conversationId="conv_1"
        onResolved={vi.fn()}
      />,
    );
    expect(screen.getByTestId("approval-narrowed-summary")).toHaveTextContent("Approved 1 of 3 · 2 left out");
  });
});
