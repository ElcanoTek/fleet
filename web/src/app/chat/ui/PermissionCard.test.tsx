import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { PermissionCard } from "./PermissionCard";
import type { PermissionRequest } from "./history";

const REQUEST: PermissionRequest = {
  id: "perm-1",
  title: "Modifying critical configuration file",
  kind: "edit",
  locations: ["/workspace/config.json"],
  options: [
    { optionId: "allow", name: "Allow this change", kind: "allow_once" },
    { optionId: "reject", name: "Skip this change", kind: "reject_once" },
  ],
  status: "pending",
};

afterEach(() => {
  vi.restoreAllMocks();
});

describe("PermissionCard", () => {
  it("renders the agent's request, its locations, and allow/deny actions", () => {
    render(
      <PermissionCard request={REQUEST} conversationId="conv-1" onResolved={() => {}} />,
    );
    expect(screen.getByTestId("permission-card")).toHaveTextContent(
      "Modifying critical configuration file",
    );
    expect(screen.getByTestId("permission-card")).toHaveTextContent("/workspace/config.json");
    // The allow-shaped option is surfaced with its name; deny is always present.
    expect(screen.getByTestId("permission-allow")).toHaveTextContent("Allow this change");
    expect(screen.getByTestId("permission-deny")).toBeInTheDocument();
  });

  it("does NOT surface a one-click approve-all: only the allow-shaped option(s) are allow buttons", () => {
    render(
      <PermissionCard request={REQUEST} conversationId="conv-1" onResolved={() => {}} />,
    );
    // Exactly one allow button (the single allow_once option). The reject_once
    // option is never an allow button — it rides the explicit Deny only.
    expect(screen.getAllByTestId("permission-allow")).toHaveLength(1);
  });

  it("POSTs the allow decision (with the chosen option id) and flips to allowed", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(new Response(JSON.stringify({ resolved: true }), { status: 200 }));
    const onResolved = vi.fn();

    render(
      <PermissionCard request={REQUEST} conversationId="conv-1" onResolved={onResolved} />,
    );
    fireEvent.click(screen.getByTestId("permission-allow"));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toBe("/api/conversations/conv-1/permissions/perm-1");
    expect(init?.method).toBe("POST");
    expect(JSON.parse(String(init?.body))).toEqual({ allowed: true, option_id: "allow" });
    await waitFor(() =>
      expect(onResolved).toHaveBeenCalledWith(expect.objectContaining({ status: "allowed" })),
    );
  });

  it("POSTs the deny decision and flips to denied", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(new Response(JSON.stringify({ resolved: true }), { status: 200 }));
    const onResolved = vi.fn();

    render(
      <PermissionCard request={REQUEST} conversationId="conv-1" onResolved={onResolved} />,
    );
    fireEvent.click(screen.getByTestId("permission-deny"));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({
      allowed: false,
      option_id: "",
    });
    await waitFor(() =>
      expect(onResolved).toHaveBeenCalledWith(expect.objectContaining({ status: "denied" })),
    );
  });

  it("renders a terminal denied state without action buttons (e.g. after a default-deny timeout)", () => {
    render(
      <PermissionCard
        request={{ ...REQUEST, status: "denied" }}
        conversationId="conv-1"
        onResolved={() => {}}
      />,
    );
    expect(screen.getByTestId("permission-card")).toHaveTextContent("Permission denied");
    expect(screen.queryByTestId("permission-allow")).toBeNull();
    expect(screen.queryByTestId("permission-deny")).toBeNull();
  });
});
