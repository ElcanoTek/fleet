import { afterEach, describe, expect, it } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { CredentialAccountAdmin } from "./CredentialAccountAdmin";
import type { McpServer } from "@/app/shared/lib/orchestratorApi";

const SERVERS: McpServer[] = [
  { name: "xandr", accounts: ["existing_acct"] },
  { name: "magnite", accounts: [] },
];

describe("CredentialAccountAdmin — read-only account seats", () => {
  afterEach(() => cleanup());

  it("lists existing account NAMES only and renders no secret input", () => {
    const { container } = render(<CredentialAccountAdmin servers={SERVERS} />);
    expect(screen.getByTestId("credential-account-xandr-existing_acct")).toBeInTheDocument();
    // No browser write path: no form, no secret field, no add/delete control.
    expect(container.querySelector("input")).toBeNull();
    expect(container.querySelector("form")).toBeNull();
    expect(screen.queryByRole("button", { name: /add account|delete/i })).toBeNull();
  });

  it("tells the reader how accounts are provisioned on the server", () => {
    render(<CredentialAccountAdmin servers={SERVERS} />);
    expect(screen.getByTestId("credential-account-howto")).toHaveTextContent(
      "fleet mcp account set <server> <account> --secret KEY=-",
    );
  });

  it("shows the empty state when no server has accounts", () => {
    render(<CredentialAccountAdmin servers={[{ name: "magnite", accounts: [] }]} />);
    expect(screen.getByText("No accounts yet.")).toBeInTheDocument();
  });

  it("previews four accounts and expands to all of them", () => {
    const many: McpServer[] = [{ name: "xandr", accounts: ["a1", "a2", "a3", "a4", "a5", "a6"] }];
    render(<CredentialAccountAdmin servers={many} />);
    expect(screen.queryByTestId("credential-account-xandr-a5")).toBeNull();
    const toggle = screen.getByRole("button", { name: /show all \(6\)/i });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(toggle);
    expect(screen.getByTestId("credential-account-xandr-a6")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /show less/i })).toHaveAttribute("aria-expanded", "true");
  });
});
