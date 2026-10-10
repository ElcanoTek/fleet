import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { ApprovalCard } from "./ApprovalCards";
import { hydrateResolvedApproval, parseApprovalCardData, type Approval, type ApprovalCardData } from "./history";

// A bundle-declared describer turns a staged critical call into a readable
// card (docs/APPROVAL-CARD-DESCRIBERS.md): a plain-words title, the records
// with their ids and links, each change as before → after, flags as badges,
// and the raw tool/arguments collapsed under Details. Without a card the
// generic arguments card renders exactly as before.

const card: ApprovalCardData = {
  title: "Update 2 deals",
  subtitle: "Raise the floor on the Q4 package",
  items: [
    {
      label: "Q4 Video",
      id: "PM-123",
      link: "https://ssp.example.com/deals/123",
      changes: [{ label: "Floor", before: "$2.00", after: "$2.50" }],
      flags: [{ code: "deal_active", label: "Deal is Active" }],
    },
    { label: "Q4 Display", settings: [{ label: "Currency", value: "USD" }], changes: [{ label: "End date", after: "2027-01-31" }] },
  ],
  footer: "Changes apply immediately.",
};

function renderCard(approval: Partial<Approval>) {
  const full: Approval = {
    id: "ap_1",
    tool: "mcp_deals_update_deal",
    status: "pending",
    summary: { tool: "mcp_deals_update_deal", args: [{ key: "deal_id", value: "PM-123" }, { key: "etag", value: "W/\"77\"" }] },
    ...approval,
  } as Approval;
  render(<ApprovalCard approval={full} conversationId="conv_1" onResolved={vi.fn()} />);
}

function manyItems(n: number) {
  return Array.from({ length: n }, (_, i) => ({ label: `Deal ${i + 1}`, id: `D-${i + 1}` }));
}

afterEach(() => cleanup());

describe("the readable approval card", () => {
  it("renders the pending layout: title, records, before → after, flags; raw args under Details", async () => {
    renderCard({ card, expiresAt: Math.floor(Date.now() / 1000) + 600, mcpServer: "deals", mcpAccount: "client_a" });
    const cardEl = screen.getByTestId("generic-action-card");
    expect(screen.getByTestId("generic-action-title")).toHaveTextContent(/^Update 2 deals$/);
    expect(cardEl).toHaveTextContent("Raise the floor on the Q4 package");
    const items = screen.getAllByTestId("approval-card-item");
    expect(items).toHaveLength(2);
    expect(within(items[0]).getByText("Q4 Video")).toBeTruthy();
    expect(within(items[0]).getByText("PM-123")).toBeTruthy();
    const link = within(items[0]).getByRole("link", { name: /Open/ });
    expect(link).toHaveAttribute("href", "https://ssp.example.com/deals/123");
    expect(link).toHaveAttribute("rel", "noopener noreferrer");
    expect(within(items[0]).getByTestId("approval-card-change")).toHaveTextContent("Floor: $2.00→$2.50");
    expect(within(items[0]).getByTestId("approval-card-flag")).toHaveTextContent("Deal is Active");
    expect(within(items[1]).getByTestId("approval-card-change")).toHaveTextContent("End date: 2027-01-31");
    expect(within(items[1]).getByTestId("approval-card-setting")).toHaveTextContent("Currency: USD");
    expect(cardEl).toHaveTextContent("Changes apply immediately.");
    // Raw tool + arguments are collapsed under Details.
    const details = screen.getByTestId("approval-card-details");
    expect(details).not.toHaveAttribute("open");
    expect(within(details).getByText("mcp_deals_update_deal")).toBeTruthy();
    expect(within(details).getByText("etag:")).toBeTruthy();
    // Buttons, countdown, seat badge and apply-all are unchanged.
    expect(screen.getByRole("button", { name: "Approve & run" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeEnabled();
    expect(await screen.findByTestId("approval-countdown")).toBeTruthy();
    expect(screen.getByTestId("approval-seat")).toHaveTextContent("client_a");
    expect(screen.getByTestId("approval-apply-all")).toBeTruthy();
  });

  it("renders every string as text", () => {
    renderCard({ card: { title: "<img src=x onerror=alert(1)>", items: [{ label: "<b>bold</b>" }] } });
    expect(screen.getByTestId("generic-action-title")).toHaveTextContent("<img src=x onerror=alert(1)>");
    expect(screen.getByText("<b>bold</b>")).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
  });

  it("collapses a long list after 10 with Show N more", () => {
    renderCard({ card: { title: "Update 14 deals", items: manyItems(14) } });
    expect(screen.getAllByTestId("approval-card-item")).toHaveLength(10);
    expect(screen.queryByTestId("approval-card-search")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show 4 more" }));
    expect(screen.getAllByTestId("approval-card-item")).toHaveLength(14);
    expect(screen.queryByTestId("approval-card-show-more")).toBeNull();
  });

  it("adds a search box above 25 records", () => {
    renderCard({ card: { title: "Update 30 deals", items: manyItems(30) } });
    const search = screen.getByTestId("approval-card-search");
    fireEvent.change(search, { target: { value: "D-27" } });
    const items = screen.getAllByTestId("approval-card-item");
    expect(items).toHaveLength(1);
    expect(items[0]).toHaveTextContent("Deal 27");
    fireEvent.change(search, { target: { value: "nothing like this" } });
    expect(screen.queryAllByTestId("approval-card-item")).toHaveLength(0);
    expect(screen.getByText(/No record matches/)).toBeTruthy();
  });

  it.each([
    ["approved", { resultText: "updated", status: "approved" as const }, /^Applied · Update 2 deals$/],
    ["failed", { resultText: "update_deal failed: 409 etag mismatch", status: "failed" as const }, /^Not applied · Update 2 deals$/],
    ["declined", { resultText: "User declined this action.", status: "rejected" as const }, /^Declined · Update 2 deals$/],
    ["timed out", { resultText: "Approval timed out — auto-denied. The action was not taken.", status: "rejected" as const }, /^Timed out — not applied · Update 2 deals$/],
    ["unknown", { resultText: "outcome lost", status: "execution_unknown" as const }, /^Outcome not recorded · Update 2 deals$/],
  ])("resolves %s to a one-line outcome with the result below", (_, patch, title) => {
    renderCard({ card, ...patch });
    expect(screen.getByTestId("generic-action-title")).toHaveTextContent(title);
    // The records leave the body for Details; the tool result shows as today.
    expect(screen.queryByTestId("approval-card-readable")).toBeNull();
    expect(screen.getByTestId("approval-card-details")).toBeTruthy();
    expect(screen.getByTestId("approval-result")).toHaveTextContent(patch.resultText);
    expect(screen.queryByRole("button", { name: "Approve & run" })).toBeNull();
  });

  it("falls back to the generic card when there is no card", () => {
    renderCard({});
    expect(screen.getByText(/ACTION REQUIRED · Run "Update deal"\?/)).toBeTruthy();
    expect(screen.queryByTestId("approval-card-details")).toBeNull();
    expect(screen.getByText("deal_id:")).toBeTruthy();
  });
});

describe("parseApprovalCardData", () => {
  it("accepts the schema and refuses anything else", () => {
    expect(parseApprovalCardData(card)).toEqual(card);
    for (const bad of [
      undefined,
      null,
      "Update 2 deals",
      [],
      { items: [] },
      { title: "", items: [] },
      { title: "x" },
      { title: "x", items: [{ id: "1" }] },
      { title: "x", items: [{ label: "a", link: "javascript:alert(1)" }] },
      { title: "x", items: [{ label: "a", link: "http://example.com" }] },
      { title: "x", items: [{ label: "a", changes: [{ label: "Floor", after: 2.5 }] }] },
      { title: "x", subtitle: 3, items: [] },
    ]) {
      expect(parseApprovalCardData(bad), JSON.stringify(bad)).toBeUndefined();
    }
  });

  it("hydrates a resolved row's card", () => {
    const a = hydrateResolvedApproval({
      approval_id: "r1",
      tool: "mcp_deals_update_deal",
      summary: {},
      status: "approved",
      is_err: false,
      card,
    });
    expect(a.card?.title).toBe("Update 2 deals");
    expect(hydrateResolvedApproval({ approval_id: "r2", tool: "t", summary: {}, status: "rejected", card: { title: 1 } }).card).toBeUndefined();
  });
});
