import { beforeEach, describe, expect, it, vi } from "vitest";

const getServerSessionMock = vi.fn();
const chatServerFetchMock = vi.fn();
const redirectMock = vi.fn((to: string) => {
  throw new Error(`redirect:${to}`);
});

vi.mock("@/app/lib/auth", () => ({
  getServerSession: () => getServerSessionMock(),
}));
vi.mock("@/app/lib/chatServer", () => ({
  chatServerFetch: (...args: unknown[]) => chatServerFetchMock(...args),
}));
vi.mock("next/navigation", () => ({
  redirect: (to: string) => redirectMock(to),
}));
vi.mock("./page-client", () => ({ PageClient: () => null }));

import Home from "./page";

// The membership gate turns a signed-in non-member into one clear /no-access
// page. It must run for every identity Fleet did not mint itself: the shared
// elcano_auth cookie AND a central-Auth (OIDC) session — Auth decides who may
// reach Fleet, Fleet's user-list decides who is a member (#1522). Password
// sessions are members by construction and skip the round-trip.
describe("chat entry membership gate", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    redirectMock.mockClear();
  });

  it.each(["oidc", "elcano"])(
    "sends a %s non-member to /no-access",
    async (source) => {
      getServerSessionMock.mockResolvedValue({
        email: "x@omc.com",
        exp: 0,
        source,
      });
      chatServerFetchMock.mockResolvedValue(
        new Response(null, { status: 403 }),
      );
      await expect(Home()).rejects.toThrow("redirect:/no-access");
      expect(chatServerFetchMock).toHaveBeenCalledWith(
        expect.objectContaining({ email: "x@omc.com" }),
        "/auth/membership",
      );
    },
  );

  it.each(["oidc", "elcano"])("renders for a %s member", async (source) => {
    getServerSessionMock.mockResolvedValue({
      email: "x@omc.com",
      exp: 0,
      source,
    });
    chatServerFetchMock.mockResolvedValue(new Response("{}", { status: 200 }));
    await expect(Home()).resolves.toBeTruthy();
    expect(redirectMock).not.toHaveBeenCalled();
  });

  it("does not check membership for a password session", async () => {
    getServerSessionMock.mockResolvedValue({
      email: "x@omc.com",
      exp: 0,
      source: "password",
    });
    await expect(Home()).resolves.toBeTruthy();
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });

  it("lets the app load when chat-server is unreachable (no trap on transient errors)", async () => {
    getServerSessionMock.mockResolvedValue({
      email: "x@omc.com",
      exp: 0,
      source: "oidc",
    });
    chatServerFetchMock.mockRejectedValue(new Error("ECONNREFUSED"));
    await expect(Home()).resolves.toBeTruthy();
    expect(redirectMock).not.toHaveBeenCalled();
  });
});
