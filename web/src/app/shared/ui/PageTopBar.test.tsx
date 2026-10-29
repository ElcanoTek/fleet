// @vitest-environment jsdom
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { PageTopBar } from "./PageTopBar";

describe("PageTopBar support contact", () => {
  it("omits the support button when no support contact is configured", () => {
    render(<PageTopBar title="Chat" onMenu={() => {}} />);
    expect(screen.queryByTestId("support-contact-button")).toBeNull();
  });

  it("renders the support button and opens the dialog with message, email, and mailto link", () => {
    render(
      <PageTopBar
        title="Chat"
        onMenu={() => {}}
        support={{
          label: "Direct Support",
          email: "support@example.com",
          title: "Direct Line to Engineering",
          message: "Reach out anytime — we keep inbox zero and reply quickly.",
          subject: "Workspace feedback",
        }}
      />,
    );

    const btn = screen.getByTestId("support-contact-button");
    expect(btn.textContent).toContain("Direct Support");

    fireEvent.click(btn);

    const dialog = screen.getByTestId("support-contact-dialog");
    expect(dialog.textContent).toContain("Direct Line to Engineering");
    expect(dialog.textContent).toContain(
      "Reach out anytime — we keep inbox zero and reply quickly.",
    );
    expect(screen.getByTestId("support-contact-email").textContent).toBe(
      "support@example.com",
    );
    expect(
      screen.getByTestId("support-contact-mailto").getAttribute("href"),
    ).toBe("mailto:support@example.com?subject=Workspace%20feedback");

    fireEvent.click(screen.getByLabelText("Close"));
    expect(screen.queryByTestId("support-contact-dialog")).toBeNull();
  });

  it("copies the email address when Copy email is clicked", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });

    render(
      <PageTopBar
        title="Operations Center"
        onMenu={() => {}}
        support={{
          label: "Direct Support",
          email: "support@example.com",
        }}
      />,
    );

    fireEvent.click(screen.getByTestId("support-contact-button"));
    fireEvent.click(screen.getByTestId("support-contact-copy"));
    expect(writeText).toHaveBeenCalledWith("support@example.com");
    await waitFor(() => {
      expect(screen.getByTestId("support-contact-copy").textContent).toBe(
        "Copied",
      );
    });
  });

  it("does not report Copied when navigator.clipboard is unavailable", () => {
    Object.assign(navigator, { clipboard: undefined });

    render(
      <PageTopBar
        title="Operations Center"
        onMenu={() => {}}
        support={{
          label: "Direct Support",
          email: "support@example.com",
        }}
      />,
    );

    fireEvent.click(screen.getByTestId("support-contact-button"));
    fireEvent.click(screen.getByTestId("support-contact-copy"));
    expect(screen.getByTestId("support-contact-copy").textContent).toBe(
      "Copy email",
    );
  });
});
