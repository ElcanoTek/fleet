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
// tests pin the one treatment across every card that can show a result.
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

function cardFor(approval: Partial<Approval> & Pick<Approval, "tool">) {
  const full: Approval = {
    id: "ap_1",
    status: "approved",
    summary: {},
    ...approval,
  } as Approval;
  render(
    <ApprovalCard approval={full} conversationId="conv_1" onResolved={() => {}} />
  );
  return screen.getByTestId("approval-result");
}

describe("a resolved approval card's result text", () => {
  it("wraps, caps its height, and scrolls — on the email card", () => {
    // The card in the reported screenshot: status approved ("Email sent ✓") with a
    // long JSON result.
    const el = cardFor({
      tool: "mcp_email_send_email",
      resultText: PAGES_DEPLOY_RESULT,
      summary: { subject: "(no subject)", content: "body", content_type: "text/plain" },
    });
    expect(el.className).toContain("break-all");
    expect(el.className).toContain("overflow-auto");
    expect(el.className).toContain("max-h-60");
    expect(el.className).toContain("whitespace-pre-wrap");
    // And the payload is still all there — capped in height, not truncated.
    expect(el.textContent).toBe(PAGES_DEPLOY_RESULT);
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
