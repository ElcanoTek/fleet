import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { CredentialAccountAdmin } from "./CredentialAccountAdmin";
import type { McpServer } from "@/app/shared/lib/orchestratorApi";

// Mock the API client so we can assert the EXACT payload sent upstream.
const createAccount = vi.fn().mockResolvedValue({ server: "xandr", account: "client_a" });
const deleteAccount = vi.fn().mockResolvedValue({ deleted: true });
vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    createAccount: (...args: unknown[]) => createAccount(...args),
    deleteAccount: (...args: unknown[]) => deleteAccount(...args),
  },
}));

const SERVERS: McpServer[] = [
  { name: "xandr", accounts: ["existing_acct"] },
  { name: "magnite", accounts: [] },
];

describe("CredentialAccountAdmin — write-only secrets", () => {
  beforeEach(() => {
    createAccount.mockClear();
    deleteAccount.mockClear();
  });
  afterEach(() => cleanup());

  it("lists existing account NAMES only and NEVER renders a secret value", () => {
    render(<CredentialAccountAdmin servers={SERVERS} />);
    // The catalog shows the account name…
    expect(screen.getByTestId("credential-account-xandr-existing_acct")).toBeInTheDocument();
    // …and there is no element anywhere echoing a secret value back. The only
    // secret inputs are the empty write-only fields in the create form.
    const secretInputs = screen.getAllByTestId(/credential-secret-value-/);
    for (const el of secretInputs) {
      expect((el as HTMLInputElement).value).toBe("");
      expect(el).toHaveAttribute("type", "password");
    }
  });

  it("starts the secret value field EMPTY (never pre-filled from storage)", () => {
    render(<CredentialAccountAdmin servers={SERVERS} />);
    const secret = screen.getByTestId("credential-secret-value-0") as HTMLInputElement;
    expect(secret.value).toBe("");
    expect(secret.type).toBe("password");
  });

  it("submits secrets as a write-only payload (key+value forwarded, never read back)", async () => {
    render(<CredentialAccountAdmin servers={SERVERS} />);
    fireEvent.change(screen.getByLabelText("Account name"), { target: { value: "client_a" } });
    fireEvent.change(screen.getByLabelText("Secret key"), { target: { value: "XANDR_API_KEY" } });
    fireEvent.change(screen.getByTestId("credential-secret-value-0"), {
      target: { value: "super-secret" },
    });
    fireEvent.click(screen.getByText("Save account"));

    await waitFor(() => expect(createAccount).toHaveBeenCalledTimes(1));
    expect(createAccount).toHaveBeenCalledWith("xandr", {
      account: "client_a",
      secrets: { XANDR_API_KEY: "super-secret" },
    });
  });

  it("drops empty secret values (never writes a key with '')", async () => {
    render(<CredentialAccountAdmin servers={SERVERS} />);
    fireEvent.change(screen.getByLabelText("Account name"), { target: { value: "client_a" } });
    fireEvent.change(screen.getByLabelText("Secret key"), { target: { value: "XANDR_API_KEY" } });
    // Leave the value EMPTY → the submit must be rejected (no createAccount).
    fireEvent.click(screen.getByText("Save account"));
    // give any async a tick
    await Promise.resolve();
    expect(createAccount).not.toHaveBeenCalled();
  });

  it("deletes an account by name", async () => {
    render(<CredentialAccountAdmin servers={SERVERS} />);
    fireEvent.click(screen.getByLabelText("Delete xandr account existing_acct"));
    await waitFor(() => expect(deleteAccount).toHaveBeenCalledWith("xandr", "existing_acct"));
  });
});
