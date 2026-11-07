import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { setBootstrapForTest } from "@/app/lib/bootstrap";

const chatExperience = vi.fn((_: { initialUserEmail?: string | null }) => null);
vi.mock("./ui/chat-experience", () => ({
  ChatExperience: (props: { initialUserEmail?: string | null }) => chatExperience(props),
}));

import ChatPage from "./page";

// The membership entry gate (#1522) moved to the server with the rest of the
// request-time work: internal/webui/spa_test.go pins that an elcano or OIDC
// non-member is redirected to /no-access, a member and a password session are
// served, and an unreachable backend never traps the user. What is left here is
// the hand-off: the server-resolved session email reaches the chat shell so
// cold boot skips its /api/session round trip.
describe("chat page", () => {
  afterEach(() => {
    setBootstrapForTest(null);
    chatExperience.mockClear();
    cleanup();
  });

  it("hands the bootstrap's session email to the chat shell", () => {
    setBootstrapForTest({ session: { email: "x@omc.com" } });
    render(<ChatPage />);
    expect(chatExperience).toHaveBeenCalledWith({ initialUserEmail: "x@omc.com" });
  });

  it("passes null when the request carried only a bearer (no cookie session)", () => {
    setBootstrapForTest({ session: null });
    render(<ChatPage />);
    expect(chatExperience).toHaveBeenCalledWith({ initialUserEmail: null });
  });
});
