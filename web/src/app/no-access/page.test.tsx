import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import NoAccessPage from "./page";

// The dead-end for a signed-in non-member: neutral copy (no Elcano wording
// on a white-labelled deployment), a retry (an administrator may have just
// added them), and the same native sign-out form every surface uses, which
// also ends a central-Auth session.
describe("/no-access", () => {
  it("offers a retry and the top-level sign-out form", () => {
    render(<NoAccessPage />);
    expect(
      screen.getByRole("heading", { name: /no access yet/i }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/hasn.t been added to this workspace yet/i),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Elcano/)).toBeNull();
    expect(screen.getByRole("link", { name: /try again/i })).toHaveAttribute(
      "href",
      "/chat",
    );
    const button = screen.getByRole("button", { name: /sign out/i });
    const form = button.closest("form")!;
    expect(form.getAttribute("method")).toBe("post");
    expect(form.getAttribute("action")).toBe("/api/auth/logout");
  });
});
