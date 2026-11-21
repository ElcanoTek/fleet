import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { AdminNoteBanner } from "./AdminNoteBanner";

// The note an administrator pinned to a chat (ADR-0117): open until someone
// acknowledges it, one line afterwards, and always saying that the assistant
// does not read it.

afterEach(cleanup);

const NOTE = { text: "Approve the card under the last reply.", author: "alex@ops.example", updated_at: 100, version: 7 };

describe("AdminNoteBanner", () => {
  it("shows an unread note in full, with who wrote it and that the assistant does not read it", () => {
    render(<AdminNoteBanner note={NOTE} onAcknowledge={() => Promise.resolve(true)} />);
    const banner = screen.getByTestId("admin-note-banner");
    expect(banner.textContent).toContain("Note from an administrator · alex@ops.example");
    expect(banner.textContent).toContain("Approve the card under the last reply.");
    expect(banner.textContent).toContain("The assistant doesn’t read it");
    expect(screen.getByRole("note", { name: "Note from an administrator" })).toBeTruthy();
  });

  it("acknowledges once, and says so when that fails", async () => {
    const ack = vi.fn().mockResolvedValueOnce(false).mockResolvedValueOnce(true);
    render(<AdminNoteBanner note={NOTE} onAcknowledge={ack} />);
    fireEvent.click(screen.getByTestId("admin-note-ack"));
    expect(await screen.findByText("Couldn’t record that. Try again.")).toBeTruthy();
    fireEvent.click(screen.getByTestId("admin-note-ack"));
    await waitFor(() => expect(ack).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByText("Couldn’t record that. Try again.")).toBeNull());
  });

  it("folds an acknowledged note to one line that opens again", () => {
    render(<AdminNoteBanner note={{ ...NOTE, seen_at: 200, seen_by: "dana@x" }} onAcknowledge={() => Promise.resolve(true)} />);
    const folded = screen.getByTestId("admin-note-banner");
    expect(folded.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByTestId("admin-note-ack")).toBeNull();
    fireEvent.click(folded);
    expect(screen.getByRole("note").textContent).toContain("Approve the card under the last reply.");
    // Read already: nothing to acknowledge, only to fold away again.
    expect(screen.queryByTestId("admin-note-ack")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Hide" }));
    expect(screen.getByTestId("admin-note-banner").getAttribute("aria-expanded")).toBe("false");
  });
});
