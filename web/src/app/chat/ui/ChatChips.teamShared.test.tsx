import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { TeamSharedChip } from "./ChatChips";

// Finding #4: a team-shared chat was badged on the rail row and the
// project-home row, and both of those are gone exactly when it matters — the
// rail collapses, and under 900px it is a drawer. The chat header is the one
// surface still on screen while you type, so the chip lives there too, and it
// is the button that opens the Share dialog.

describe("TeamSharedChip", () => {
  it("is a real button that names the audience and opens sharing", () => {
    const onClick = vi.fn();
    render(<TeamSharedChip audience="Testing" onClick={onClick} />);
    const chip = screen.getByRole("button", {
      name: "Shared with Testing — open sharing",
    });
    // Keyboard-reachable by construction (a <button>, not a div), and it
    // carries the focus ring the rest of the header uses.
    expect(chip.tagName).toBe("BUTTON");
    expect(chip.className).toContain("focus-visible:shadow-[var(--focus-ring)]");
    fireEvent.click(chip);
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it("falls back to 'your team' rather than guessing a team name", () => {
    render(<TeamSharedChip onClick={() => {}} />);
    expect(
      screen.getByRole("button", { name: /Shared with your team/ }),
    ).toBeInTheDocument();
  });

  it("uses the project home's wording and the two-people glyph", () => {
    const { container } = render(
      <TeamSharedChip audience="Testing" onClick={() => {}} />,
    );
    // Same visible label as the project home's chip ("Shared with team"),
    // same glyph, and never the bare word "shared" (docs/TEAM-SHARING.md's
    // vocabulary table: a badge is always labeled with its audience).
    expect(screen.getByText("Shared with team")).toBeInTheDocument();
    expect(container.querySelector("svg")).not.toBeNull();
  });

  it("keeps the glyph at phone widths, where only the text is dropped", () => {
    // The label collapses the way the Lockdown badge next door does — the
    // chip itself must never be the thing that disappears, since the narrow
    // layout is precisely the case the finding is about. The accessible name
    // carries the audience at every width.
    render(<TeamSharedChip audience="Testing" onClick={() => {}} />);
    expect(screen.getByText("Shared with team").className).toContain(
      "hidden sm:inline",
    );
    expect(
      screen.getByRole("button", { name: /Shared with Testing/ }).className,
    ).not.toContain("hidden");
  });
});
