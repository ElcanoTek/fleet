import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { InjectedContextNote } from "./InjectedContextNote";

const SHARED_LIBRARY_BLOCK =
  "\n\n---\n**Shared file library (files your administrator published):**\n- `rate_card.csv` (12 KiB)\n";

describe("InjectedContextNote", () => {
  it("renders nothing when there is no injected context", () => {
    const { container } = render(<InjectedContextNote text={undefined} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing for a blank column value", () => {
    const { container } = render(<InjectedContextNote text={"  \n\t "} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("is collapsed by default and names what it is", () => {
    render(<InjectedContextNote text={SHARED_LIBRARY_BLOCK} />);
    const toggle = screen.getByRole("button", {
      name: /Context fleet added — not typed by the sender/,
    });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveTextContent("Show");
    // The block is present for assistive tech to reach on expand, but not
    // shown: the reader did not come here to read fleet's plumbing.
    expect(screen.getByText(/Shared file library/)).not.toBeVisible();
  });

  it("expands and collapses on click", async () => {
    const user = userEvent.setup();
    render(<InjectedContextNote text={SHARED_LIBRARY_BLOCK} />);
    const toggle = screen.getByRole("button", { name: /Context fleet added/ });
    await user.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(toggle).toHaveTextContent("Hide");
    expect(screen.getByText(/Shared file library/)).toBeVisible();
    await user.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(screen.getByText(/Shared file library/)).not.toBeVisible();
  });

  it("is keyboard-operable and labels the panel it controls", async () => {
    const user = userEvent.setup();
    render(<InjectedContextNote text={SHARED_LIBRARY_BLOCK} />);
    const toggle = screen.getByRole("button", { name: /Context fleet added/ });
    await user.tab();
    expect(toggle).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    await user.keyboard(" ");
    expect(toggle).toHaveAttribute("aria-expanded", "false");

    const panelId = toggle.getAttribute("aria-controls");
    expect(panelId).toBeTruthy();
    expect(document.getElementById(panelId as string)).not.toBeNull();
  });

  it("shows the context verbatim rather than as rendered prose", () => {
    render(<InjectedContextNote text={SHARED_LIBRARY_BLOCK} />);
    // The markdown markers are still literal text — the panel's job is to show
    // what the model received, not to re-typeset it as something a human wrote.
    expect(
      screen.getByText(
        /\*\*Shared file library \(files your administrator published\):\*\*/,
      ),
    ).toBeInTheDocument();
  });
});
