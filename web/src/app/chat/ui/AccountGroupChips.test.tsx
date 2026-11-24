import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { AccountGroupChips } from "./AccountGroupChips";
import type { AccountGroups } from "./accountGroups";

const rows = [
  { name: "ix", display_name: "Index Exchange", enabled: true, accounts: ["twc", "arena"] },
  { name: "ox", display_name: "OpenX", enabled: false, accounts: ["twc"] },
];

const groups: AccountGroups = {
  label: "Clients",
  groups: [
    { id: "twc", name: "The Weather Company", accounts: [
      { server: "ix", account: "twc", status: "ok" },
      { server: "ox", account: "twc", status: "ok" },
    ] },
    { id: "arena", name: "The Arena Group", accounts: [
      { server: "ix", account: "arena", status: "ok" },
      { server: "pm", account: "arena", status: "connector_missing" },
      { server: "ox", account: "arena", status: "account_missing" },
    ] },
    { id: "gone", name: "Nowhere", accounts: [{ server: "pm", account: "x", status: "connector_missing" }] },
  ],
};

describe("AccountGroupChips", () => {
  it("renders nothing without groups", () => {
    const { container } = render(
      <AccountGroupChips accountGroups={null} rows={rows} query="" appName="Elcano" onApply={() => {}} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("a chip is a toggle that hands back the changed rows", () => {
    const onApply = vi.fn();
    render(<AccountGroupChips accountGroups={groups} rows={rows} query="" appName="Elcano" onApply={onApply} />);
    expect(screen.getByRole("group", { name: "Clients" })).toBeInTheDocument();
    const twc = screen.getByRole("button", { name: "The Weather Company" });
    expect(twc).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(twc);
    expect(onApply).toHaveBeenCalledTimes(1);
    const next = onApply.mock.calls[0][0] as { name: string; enabled: boolean; account?: string }[];
    expect(next.map((r) => [r.name, r.enabled, r.account])).toEqual([
      ["ix", true, "twc"],
      ["ox", true, "twc"],
    ]);
  });

  it("reads as pressed when every usable member is on", () => {
    const on = [
      { ...rows[0], account: "twc" },
      { ...rows[1], enabled: true, account: "twc" },
    ];
    render(<AccountGroupChips accountGroups={groups} rows={on} query="" appName="Elcano" onApply={() => {}} />);
    expect(screen.getByRole("button", { name: "The Weather Company" })).toHaveAttribute("aria-pressed", "true");
  });

  it("flags a group with members that are not set up here, and says which", () => {
    render(<AccountGroupChips accountGroups={groups} rows={rows} query="" appName="Elcano" onApply={() => {}} />);
    const arena = screen.getByRole("button", { name: /The Arena Group/ });
    expect(arena).toHaveAttribute("data-flagged", "true");
    expect(arena).toHaveAccessibleName("The Arena Group. Not set up here: pm, OpenX (account arena).");
    expect(arena).toHaveAttribute("title", "Not set up here: pm, OpenX (account arena).");
    expect(arena).not.toHaveAttribute("aria-disabled");
  });

  it("says when the house account is out of reach because the user's default is a named one", () => {
    const house: AccountGroups = {
      label: "Clients",
      groups: [{ id: "house", name: "House", accounts: [{ server: "ix", account: "", status: "ok" }, { server: "ox", account: "", status: "ok" }] }],
    };
    const mine = [{ ...rows[0], default_account: "twc" }, rows[1]];
    render(<AccountGroupChips accountGroups={house} rows={mine} query="" appName="Elcano" onApply={() => {}} />);
    expect(screen.getByRole("button", { name: /House/ })).toHaveAttribute(
      "title",
      "Not set up here: Index Exchange (your default account there is twc, not Elcano).",
    );
  });

  it("a group with nothing usable here cannot be pressed", () => {
    const onApply = vi.fn();
    render(<AccountGroupChips accountGroups={groups} rows={rows} query="" appName="Elcano" onApply={onApply} />);
    const gone = screen.getByRole("button", { name: /Nowhere/ });
    expect(gone).toHaveAttribute("aria-disabled", "true");
    expect(gone).toHaveAccessibleName("Nowhere. Not set up here: pm.");
    fireEvent.click(gone);
    expect(onApply).not.toHaveBeenCalled();
  });

  it("filters with the picker's search and uses a generic heading when the bundle gives none", () => {
    render(
      <AccountGroupChips
        accountGroups={{ ...groups, label: "" }}
        rows={rows}
        query="weather"
        appName="Elcano"
        onApply={() => {}}
      />,
    );
    expect(screen.getByRole("group", { name: "Account groups" })).toBeInTheDocument();
    expect(screen.getAllByRole("button")).toHaveLength(1);
  });
});
