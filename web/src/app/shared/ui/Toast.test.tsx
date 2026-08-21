import { describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { ToastProvider, useToast } from "./Toast";

// Toast a11y contract (cohort-3 a11y pass): the toast is a live region that
// announces itself, and dismissal is a real button — previously the whole
// toast div carried the onClick, which left keyboard and switch users with no
// way to clear a toast at all.

function Trigger({ message = "Saved", type }: { message?: string; type?: "success" | "error" }) {
  const { showToast } = useToast();
  return (
    <button type="button" onClick={() => showToast(message, type, 0)}>
      fire
    </button>
  );
}

function renderWithProvider(ui: React.ReactNode) {
  return render(<ToastProvider>{ui}</ToastProvider>);
}

describe("ToastProvider", () => {
  it("announces the message in a live region", () => {
    renderWithProvider(<Trigger message="Dataset saved" />);
    fireEvent.click(screen.getByRole("button", { name: "fire" }));

    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("Dataset saved");
    // The container is the polite live region the alert lives inside.
    expect(alert.parentElement).toHaveAttribute("aria-live", "polite");
    expect(alert.parentElement).toHaveAttribute("aria-atomic", "true");
  });

  it("exposes a keyboard-operable dismiss control with an accessible name", () => {
    renderWithProvider(<Trigger />);
    fireEvent.click(screen.getByRole("button", { name: "fire" }));

    const dismiss = screen.getByRole("button", { name: "Dismiss notification" });
    // A real <button>: focusable, activated by Enter/Space by the UA, and
    // type="button" so it can never submit an enclosing form.
    expect(dismiss.tagName).toBe("BUTTON");
    expect(dismiss).toHaveAttribute("type", "button");

    dismiss.focus();
    expect(document.activeElement).toBe(dismiss);

    fireEvent.click(dismiss);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps the toast body non-interactive (dismissal is the button only)", () => {
    renderWithProvider(<Trigger message="Still here" />);
    fireEvent.click(screen.getByRole("button", { name: "fire" }));

    fireEvent.click(screen.getByRole("alert"));
    expect(screen.getByRole("alert")).toHaveTextContent("Still here");
  });

  it("preserves the type-specific toast classes", () => {
    renderWithProvider(<Trigger type="error" />);
    fireEvent.click(screen.getByRole("button", { name: "fire" }));

    const alert = screen.getByRole("alert");
    expect(alert.className).toContain("toast");
    expect(alert.className).toContain("toast--error");
  });

  it("dismisses only the clicked toast when several are stacked", () => {
    function Multi() {
      const { showToast } = useToast();
      return (
        <button
          type="button"
          onClick={() => {
            showToast("first", "info", 0);
            showToast("second", "info", 0);
          }}
        >
          fire
        </button>
      );
    }
    renderWithProvider(<Multi />);
    fireEvent.click(screen.getByRole("button", { name: "fire" }));

    expect(screen.getAllByRole("alert")).toHaveLength(2);
    const [firstDismiss] = screen.getAllByRole("button", { name: "Dismiss notification" });
    fireEvent.click(firstDismiss);

    const remaining = screen.getAllByRole("alert");
    expect(remaining).toHaveLength(1);
    expect(remaining[0]).toHaveTextContent("second");
  });
});
