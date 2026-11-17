import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { AutonomousRiskModal } from "./AutonomousRiskModal";

describe("AutonomousRiskModal", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("explains what runs automatically and what still requires approval, and cancels cleanly", () => {
    const onAccepted = vi.fn();
    const onClose = vi.fn();

    render(
      <AutonomousRiskModal
        open={true}
        onAccepted={onAccepted}
        onClose={onClose}
      />,
    );

    const modal = screen.getByTestId("autonomous-risk-modal");
    expect(modal).toHaveAttribute("role", "dialog");
    expect(modal.getAttribute("aria-describedby")).toBeTruthy();
    expect(modal).toHaveTextContent("Enable Autonomous mode");
    expect(modal).toHaveTextContent("Are you sure you want to use Autonomous mode?");
    expect(modal).toHaveTextContent("Bundle-configured connector actions");
    expect(modal).toHaveTextContent("10s countdown");
    expect(modal).toHaveTextContent("marks as blocked");
    expect(modal).toHaveTextContent("git push");

    fireEvent.click(screen.getByTestId("autonomous-risk-cancel"));
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(onAccepted).not.toHaveBeenCalled();
  });

  it("POSTs /api/chat/autonomous-accept and invokes onAccepted on confirmation", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({ autonomous_accepted: true, autonomous_policy_version: 1 }),
        { status: 200 },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    const onAccepted = vi.fn();
    const onClose = vi.fn();

    render(
      <AutonomousRiskModal
        open={true}
        onAccepted={onAccepted}
        onClose={onClose}
      />,
    );

    fireEvent.click(screen.getByTestId("autonomous-risk-accept"));
    await waitFor(() => expect(onAccepted).toHaveBeenCalledTimes(1));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][0]).toBe("/api/chat/autonomous-accept");
    expect(fetchMock.mock.calls[0][1]?.method).toBe("POST");
    expect(onClose).not.toHaveBeenCalled();
  });
});
