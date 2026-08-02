import { describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { ToolChip, isEmailSendTool } from "./ToolChips";
import type { ToolCall } from "./history";

// The send_email result used to land in the transcript as a raw JSON dump —
// provider status, message id, and a paragraph of HTML-lint prose — directly
// under the approval card the user had just clicked Send on. These tests pin
// the human-readable form: an outcome badge, a failed send that says so
// instead of "queued", and the payload still reachable behind a disclosure.

function chipFor(name: string, resultText: string, state: ToolCall["state"] = "done") {
  const tc: ToolCall = { id: "call_1", name, input: "{}", resultText, state };
  render(<ToolChip tc={tc} taskTrackerDisplay={null} />);
  // The chip renders collapsed; the result view only mounts once expanded.
  fireEvent.click(screen.getByRole("button"));
}

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

describe("isEmailSendTool", () => {
  it("matches the built-in verb and the MCP variants", () => {
    expect(isEmailSendTool("send_email")).toBe(true);
    expect(isEmailSendTool("mcp_sendgrid_send_email")).toBe(true);
    expect(isEmailSendTool("mcp_mailbux_send_email")).toBe(true);
  });

  it("leaves email-adjacent tools on the raw view", () => {
    expect(isEmailSendTool("preview_email")).toBe(false);
    expect(isEmailSendTool("send_email_draft")).toBe(false);
    expect(isEmailSendTool("bash")).toBe(false);
  });
});

describe("send_email result card", () => {
  it("summarizes a queued send and hides the payload behind a disclosure", () => {
    chipFor("mcp_sendgrid_send_email", QUEUED);

    expect(screen.getByText("Queued for delivery")).toBeTruthy();
    expect(screen.getByText("1 formatting note")).toBeTruthy();
    // The raw payload is still there — inside the collapsed <details>, not
    // spilled into the transcript.
    const details = document.querySelector("details");
    expect(details).toBeTruthy();
    expect(details?.hasAttribute("open")).toBe(false);
    expect(details?.textContent).toContain("8RTfMNrQQKyC3M6iAD6QHg");
    expect(details?.textContent).toContain("EO002");
  });

  it("reports a provider rejection as not sent, with the reason", () => {
    chipFor(
      "mcp_sendgrid_send_email",
      JSON.stringify({
        error: "SendGrid API error (403): sender identity not verified",
        status_code: 403,
      }),
    );

    expect(screen.getByText("Not sent")).toBeTruthy();
    expect(screen.getByTestId("email-send-detail").textContent).toContain(
      "sender identity not verified",
    );
    expect(screen.queryByText("Queued for delivery")).toBeNull();
  });

  it("does not claim success on a non-2xx status without an error key", () => {
    // The regression this guards: reading only is_err (false here, because the
    // tool returned a payload rather than raising) printed "queued" over a
    // send the provider had refused.
    chipFor("mcp_sendgrid_send_email", JSON.stringify({ status_code: 500 }));

    expect(screen.getByText("Not sent")).toBeTruthy();
    expect(screen.getByText(/returned status 500/)).toBeTruthy();
  });

  it("names a ledger-suppressed duplicate instead of a second send", () => {
    chipFor(
      "mcp_sendgrid_send_email",
      JSON.stringify({
        status_code: 202,
        status: "queued",
        duplicate_suppressed: true,
        note: "duplicate suppressed: an identical email was already sent",
      }),
    );

    expect(screen.getByText("Already sent")).toBeTruthy();
    expect(screen.getByTestId("email-send-detail").textContent).toContain(
      "identical email was already sent",
    );
  });

  it("explains the staged-for-approval placeholder in plain language", () => {
    chipFor(
      "mcp_sendgrid_send_email",
      "APPROVAL_REQUIRED: this send_email call has been staged for explicit user approval (approval_id=b6b12549). Do NOT retry.",
      "error",
    );

    expect(screen.getByText(/Waiting for your approval/)).toBeTruthy();
    expect(screen.queryByText(/APPROVAL_REQUIRED/)).toBeNull();
  });

  it("keeps the raw block for a non-JSON result", () => {
    chipFor("mcp_sendgrid_send_email", "connection reset by peer", "error");

    expect(screen.getByText("connection reset by peer")).toBeTruthy();
    expect(document.querySelector("details")).toBeNull();
  });
});
