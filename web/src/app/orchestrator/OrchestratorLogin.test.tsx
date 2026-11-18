import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { OrchestratorLogin } from "./OrchestratorLogin";

// The Operations Center's sign-in card is the safety net for a session the
// server revoked after the page loaded. Both of its sign-in entries carry the
// page it is on — a task link's ?task= included — so the visitor comes back
// to it (internal/webui/returnto.go).

afterEach(() => {
  cleanup();
  window.history.replaceState(null, "", "/");
});

describe("OrchestratorLogin", () => {
  const link = "/orchestrator?task=0f8fad5b-d9cb-469f-a165-70867728950e";

  it("sends the Elcano email hand-off back to the task link", () => {
    window.history.replaceState(null, "", link);
    render(<OrchestratorLogin magicLinkLoginEnabled />);
    expect(screen.getByRole("link", { name: "Sign in with your Elcano email" })).toHaveAttribute(
      "href",
      `/api/orchestrator/auth/elcano-login?return_to=${encodeURIComponent(link)}`,
    );
  });

  it("sends sign-in via Chat back to the task link", () => {
    window.history.replaceState(null, "", link);
    render(<OrchestratorLogin magicLinkLoginEnabled={false} />);
    expect(screen.getByRole("link", { name: "Sign in via Chat" })).toHaveAttribute(
      "href",
      `/login?return_to=${encodeURIComponent(link)}`,
    );
  });
});
