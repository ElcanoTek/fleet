import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { ApprovalCard } from "./ApprovalCards";
import type { Approval } from "./history";

// A resolved approval card renders whatever text the tool came back with, and a
// tool result is arbitrary text of arbitrary length. That was rendered four
// different ways in ApprovalCards.tsx: the bash card capped and wrapped it, the
// schedule card wrapped without a cap, and the email and advanced-model cards used
// a bare <p> with no wrapping at all.
//
// Nobody expected an *email* result to be long — until one carried a 4 KB Pages
// deploy payload. The card rendered it unwrapped and unbounded: minified JSON
// clipped mid-token at the card edge, no scrollbar, filling the viewport. These
// tests pin that raw-text treatment on every card that can show one — and, for
// the email card, that a provider JSON payload is humanized instead of shown
// raw at all (the second half of this file).
//
// break-all rather than break-words is load-bearing: minified JSON contains no
// spaces, so there is nothing for word-level wrapping to break on.

const PAGES_DEPLOY_RESULT = JSON.stringify({
  version: { id: "143", page_id: "39", content_sha256: "c60b8ef7147df77f7be314f48fa9e04039a7c3675591de55c03241da9a917080" },
  status: "approved",
  render_mode: "themed",
  author: "fleet-dev-2",
  config_schema: {
    $id: "CONFIG",
    description:
      "Per-campaign settings, fixed for the life of the campaign. Everything that changes on a refresh lives in the data payload instead.",
    properties: { campaign: { type: "string", minLength: 1, maxLength: 200 } },
  },
});

function renderCard(approval: Partial<Approval> & Pick<Approval, "tool">) {
  const full: Approval = {
    id: "ap_1",
    status: "approved",
    summary: {},
    ...approval,
  } as Approval;
  render(
    <ApprovalCard approval={full} conversationId="conv_1" onResolved={() => {}} />
  );
}

function cardFor(approval: Partial<Approval> & Pick<Approval, "tool">) {
  renderCard(approval);
  return screen.getByTestId("approval-result");
}

describe("a resolved approval card's result text", () => {
  // The email card no longer renders a JSON payload raw — see the
  // "a resolved email approval's result" block below. A result that is NOT
  // the provider payload (a network error string) keeps the raw treatment.
  it("wraps, caps its height, and scrolls — on the email card's non-JSON result", () => {
    const el = cardFor({
      tool: "mcp_email_send_email",
      status: "failed",
      resultText: "x".repeat(5000),
      summary: { subject: "(no subject)", content: "body", content_type: "text/plain" },
    });
    expect(el.className).toContain("break-all");
    expect(el.className).toContain("overflow-auto");
    expect(el.className).toContain("max-h-60");
    expect(el.className).toContain("whitespace-pre-wrap");
  });

  it("uses the same treatment on the bash card", () => {
    const el = cardFor({
      tool: "bash",
      resultText: "x".repeat(5000),
      summary: { command: "ls -la", working_dir: "/tmp" },
    });
    expect(el.className).toContain("break-all");
    expect(el.className).toContain("overflow-auto");
    expect(el.className).toContain("max-h-60");
  });

  it("uses the same treatment on the suggest-advanced-model card", () => {
    const el = cardFor({
      tool: "suggest_advanced_model",
      resultText: PAGES_DEPLOY_RESULT,
      summary: { reason: "This needs a stronger model.", recommend_model: "anthropic/claude-opus-5" },
    });
    expect(el.className).toContain("break-all");
    expect(el.className).toContain("overflow-auto");
    expect(el.className).toContain("max-h-60");
  });

  it("renders nothing when there is no result text", () => {
    render(
      <ApprovalCard
        approval={{ id: "ap_2", tool: "bash", status: "approved", summary: { command: "true" } } as Approval}
        conversationId="conv_1"
        onResolved={() => {}}
      />
    );
    expect(screen.queryByTestId("approval-result")).toBeNull();
  });
});

// The provider's JSON answer to a successful send used to land raw under
// "Email sent ✓" — a wall of {"status_code":202,...} that read as an error to
// exactly the non-technical users the approval flow exists for. The card now
// reuses the transcript chip's humanized outcome: a status badge, with the
// payload intact behind a collapsed "Delivery details" disclosure.
describe("a resolved email approval's result", () => {
  const QUEUED = JSON.stringify({
    status_code: 202,
    message_id: "8RTfMNrQQKyC3M6iAD6QHg",
    status: "queued",
    html_validated: true,
    validation_warnings: [
      {
        rule: "EO002",
        severity: "warning",
        message: "<td> uses a single-side CSS border.",
        hint: "Use a filled cell instead.",
      },
    ],
  });

  it("summarizes a sent email instead of dumping the provider JSON", () => {
    renderCard({
      tool: "mcp_mailbux_send_email",
      status: "approved",
      resultText: QUEUED,
      summary: { subject: "Weekly report", content: "<p>hi</p>", content_type: "text/html" },
    });
    expect(screen.getByText("Queued for delivery")).toBeTruthy();
    expect(screen.getByText("1 formatting note")).toBeTruthy();
    // No raw dump on the card face…
    expect(screen.queryByTestId("approval-result")).toBeNull();
    // …but the payload is still reachable, collapsed behind the disclosure.
    const details = document.querySelector("details");
    expect(details).toBeTruthy();
    expect(details?.hasAttribute("open")).toBe(false);
    expect(details?.textContent).toContain("8RTfMNrQQKyC3M6iAD6QHg");
    expect(details?.textContent).toContain("EO002");
  });

  it("reports a provider rejection as not sent, with the reason", () => {
    renderCard({
      tool: "mcp_mailbux_send_email",
      status: "failed",
      resultText: JSON.stringify({
        error: "provider error (403): sender identity not verified",
        status_code: 403,
      }),
      summary: { subject: "Weekly report", content: "body", content_type: "text/plain" },
    });
    expect(screen.getByText("Not sent")).toBeTruthy();
    expect(screen.getByTestId("email-send-detail").textContent).toContain(
      "sender identity not verified",
    );
    expect(screen.queryByTestId("approval-result")).toBeNull();
  });

  it("handles the long-JSON regression payload without a raw dump", () => {
    renderCard({
      tool: "mcp_email_send_email",
      status: "approved",
      resultText: PAGES_DEPLOY_RESULT,
      summary: { subject: "(no subject)", content: "body", content_type: "text/plain" },
    });
    expect(screen.queryByTestId("approval-result")).toBeNull();
    const details = document.querySelector("details");
    // The payload is all still there — behind the disclosure, not truncated.
    expect(details?.textContent).toContain(
      "c60b8ef7147df77f7be314f48fa9e04039a7c3675591de55c03241da9a917080",
    );
  });

  it("keeps the raw view on the preview card's result", () => {
    renderCard({
      tool: "preview_email",
      status: "failed",
      resultText: PAGES_DEPLOY_RESULT,
      summary: { subject: "(no subject)", content: "body", content_type: "text/plain" },
    });
    expect(screen.getByTestId("approval-result").textContent).toBe(PAGES_DEPLOY_RESULT);
  });
});
