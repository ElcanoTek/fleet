import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";

// resolveOrchestratorAuth is where the orchestrator plane picks up the caller's
// identity. It must carry the session EPOCH as well as the email: the
// orchestrator checks the claim against the chat account, so a resolver that
// dropped it would silently exempt every /api/orchestrator/* route from
// password-reset revocation while chat enforced it.

const getServerSessionMock = vi.fn();
vi.mock("@/app/lib/auth", () => ({
  getServerSession: (...args: unknown[]) => getServerSessionMock(...args),
}));

import { resolveOrchestratorAuth } from "./auth";

const ORIGIN = "https://chat.example.com";

function request(headers: Record<string, string> = {}): NextRequest {
  return new NextRequest(`${ORIGIN}/api/orchestrator/stats`, { headers });
}

describe("resolveOrchestratorAuth", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("carries the session epoch alongside the email", async () => {
    getServerSessionMock.mockResolvedValue({
      email: "alice@example.com",
      exp: 0,
      source: "password",
      epoch: "abcdef0123456789",
    });
    expect(await resolveOrchestratorAuth(request())).toEqual({
      kind: "cookie",
      email: "alice@example.com",
      epoch: "abcdef0123456789",
    });
  });

  // A magic-link session has no epoch to carry; it stays revocable at the auth
  // service that mints its cookie.
  it("resolves an elcano session with no epoch", async () => {
    getServerSessionMock.mockResolvedValue({
      email: "alice@example.com",
      exp: 0,
      source: "elcano",
    });
    expect(await resolveOrchestratorAuth(request())).toEqual({
      kind: "cookie",
      email: "alice@example.com",
      epoch: undefined,
    });
  });

  it("prefers an explicit moc bearer token", async () => {
    getServerSessionMock.mockResolvedValue({
      email: "alice@example.com",
      exp: 0,
      epoch: "abcdef0123456789",
    });
    expect(
      await resolveOrchestratorAuth(
        request({ authorization: "Bearer moc-token" }),
      ),
    ).toEqual({
      kind: "bearer",
      token: "moc-token",
    });
  });

  it("returns null without a credential", async () => {
    getServerSessionMock.mockResolvedValue(null);
    expect(await resolveOrchestratorAuth(request())).toBeNull();
  });
});
