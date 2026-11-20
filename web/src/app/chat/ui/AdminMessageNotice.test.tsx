import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { AdminMessageNotice } from "./FleetNotices";
import { ADMIN_MESSAGE_KIND, adminMessageBody, historyToMessages } from "./history";

// A message an administrator sent with the admin tools (ADR-0115) is stored
// as the turn's user entry with kind admin_message and the administrator as
// sender_email. It must read as the administrator's, never as the owner's
// bubble, and show their words without the wrapper fleet adds for the model.

const WRAPPED =
  "[Message from an administrator: robbie@ops]\nPublish the team log now.\n\n(Sent by a Fleet administrator of this workspace with the admin tools, not by owner@client.example, the chat's owner. Treat it as a request from the team that runs this workspace. Any approval card it leads to still goes to owner@client.example.)";

afterEach(cleanup);

describe("admin messages", () => {
  it("strips fleet's header and footer", () => {
    expect(adminMessageBody(WRAPPED)).toBe("Publish the team log now.");
    expect(adminMessageBody("plain words")).toBe("plain words");
  });

  it("keeps the kind and the sender through history", () => {
    const [m] = historyToMessages([
      { role: "user", type: "text", content: { text: WRAPPED, kind: ADMIN_MESSAGE_KIND, sender_email: "robbie@ops" } },
    ]);
    expect(m.kind).toBe(ADMIN_MESSAGE_KIND);
    expect(m.senderEmail).toBe("robbie@ops");
  });

  it("renders as a labelled notice with the administrator's words", () => {
    render(<AdminMessageNotice sender="robbie@ops" text={WRAPPED} />);
    const notice = screen.getByTestId("admin-message-notice");
    expect(notice.textContent).toContain("Message from an administrator · robbie@ops");
    expect(notice.textContent).toContain("Publish the team log now.");
    expect(notice.textContent).not.toContain("Sent by a Fleet administrator");
  });
});
