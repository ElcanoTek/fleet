import { describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { ApprovalCard, emailAttachmentNames } from "./ApprovalCards";
import type { Approval } from "./history";

// The send-email approval card is the last look a person gets before a
// client-facing email leaves. Two things it used to hide: who was only copied
// (To, Cc and Bcc were folded into one "To:" line) and which files ride along
// (the server froze `attachments` into the summary, the card never read it).

function renderEmailCard(summary: Approval["summary"], status: Approval["status"] = "pending") {
  const approval = {
    id: "ap_email",
    tool: "mcp_ses_outbound_send_email",
    status,
    summary: { subject: "Weekly report", content: "<p>hi</p>", content_type: "text/html", ...summary },
  } as Approval;
  render(<ApprovalCard approval={approval} conversationId="conv_1" onResolved={() => {}} />);
}

describe("the email approval card's envelope", () => {
  it("puts To, Cc and Bcc on separate lines", () => {
    renderEmailCard({
      to: ["a@example.com", "b@example.com"],
      cc: ["c@example.com"],
      bcc: "d@example.com",
    });
    expect(screen.getByTestId("email-to").textContent).toBe("To: a@example.com, b@example.com");
    expect(screen.getByTestId("email-cc").textContent).toBe("Cc: c@example.com");
    expect(screen.getByTestId("email-bcc").textContent).toBe("Bcc: d@example.com");
  });

  it("omits the Cc and Bcc lines when there are none", () => {
    renderEmailCard({ to: "a@example.com" });
    expect(screen.queryByTestId("email-cc")).toBeNull();
    expect(screen.queryByTestId("email-bcc")).toBeNull();
  });

  it("lists every outbound file, inline ones included, even when the body is collapsed", () => {
    renderEmailCard(
      {
        to: "a@example.com",
        attachments: ["/workspace/out/report.csv", { path: "/workspace/out/deck.pdf" }],
        inline_attachments: [{ path: "/workspace/chart.png", cid: "chart" }],
      },
      "approved",
    );
    const box = screen.getByTestId("email-attachments");
    expect(within(box).getByText("📎 3 attachments")).toBeTruthy();
    expect(within(box).getByText("report.csv")).toBeTruthy();
    expect(within(box).getByText("deck.pdf")).toBeTruthy();
    expect(within(box).getByText("/workspace/out/report.csv")).toBeTruthy();
    // Inline files are sent too (an unreferenced cid still goes out), so they
    // are listed as well, tagged.
    expect(within(box).getByText("chart.png")).toBeTruthy();
    expect(within(box).getAllByText("inline")).toHaveLength(1);
  });

  it("shows no attachment block for an email without files", () => {
    renderEmailCard({ to: "a@example.com" });
    expect(screen.queryByTestId("email-attachments")).toBeNull();
  });
});

describe("emailAttachmentNames", () => {
  it("accepts every shape the senders accept and skips junk", () => {
    expect(
      emailAttachmentNames([
        "report.csv",
        { path: "C:\\out\\a.xlsx" },
        { path: "/w/x.bin", filename: "Q3 Report.xlsx" },
        { file: "/w/chart.png", cid: "chart" },
        { nope: 1 },
        42,
      ]),
    ).toEqual([
      { name: "report.csv", path: "report.csv" },
      { name: "a.xlsx", path: "C:\\out\\a.xlsx" },
      { name: "Q3 Report.xlsx", path: "/w/x.bin" },
      { name: "chart.png", path: "/w/chart.png" },
    ]);
    expect(emailAttachmentNames("/w/solo.csv")).toEqual([{ name: "solo.csv", path: "/w/solo.csv" }]);
    expect(emailAttachmentNames(undefined)).toEqual([]);
  });
});
