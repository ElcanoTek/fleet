import { act, fireEvent, render, screen } from "@testing-library/react";
import { readFileSync } from "node:fs";
import path from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { __resetDeployDriftForTests } from "./deployDrift";
import { UpdateNotice } from "./UpdateNotice";

// The update notice is app-wide: it shows on any signed-in page (not just
// chat), names the deployment, offers the reload, and owns the tab's only
// deploy probe.

const nav = vi.hoisted(() => ({ pathname: "/orchestrator" }));
vi.mock("@/app/lib/navigation", () => ({ usePathname: () => nav.pathname }));

function versionResponse(buildId: string) {
  return new Response(JSON.stringify({ buildId }), { status: 200, headers: { "Content-Type": "application/json" } });
}

describe("UpdateNotice", () => {
  const originalLocation = window.location;
  let fetchMock: ReturnType<typeof vi.fn>;
  let reload: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    __resetDeployDriftForTests();
    nav.pathname = "/orchestrator";
    vi.stubEnv("VITE_BUILD_ID", "client-build");
    fetchMock = vi.fn(async () => versionResponse("server-build"));
    vi.stubGlobal("fetch", fetchMock);
    reload = vi.fn();
    Object.defineProperty(window, "location", { configurable: true, value: { ...originalLocation, reload } });
  });

  afterEach(() => {
    Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
    vi.unstubAllEnvs();
    vi.unstubAllGlobals();
  });

  it("shows on a non-chat page once a newer build is deployed, and reloads on request", async () => {
    render(<UpdateNotice appName="Reklaim" />);
    const notice = await screen.findByTestId("update-notice");
    expect(notice).toHaveTextContent("Reklaim was updated");
    expect(fetchMock).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("can be dismissed", async () => {
    render(<UpdateNotice appName="Fleet" />);
    await screen.findByTestId("update-notice");
    fireEvent.click(screen.getByRole("button", { name: "Dismiss update notice" }));
    expect(screen.queryByTestId("update-notice")).not.toBeInTheDocument();
  });

  it("stays hidden while the builds match", async () => {
    fetchMock.mockResolvedValue(versionResponse("client-build"));
    render(<UpdateNotice appName="Fleet" />);
    await act(async () => {});
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(screen.queryByTestId("update-notice")).not.toBeInTheDocument();
  });

  it("does not probe on the signed-out pages", async () => {
    for (const pathname of ["/login", "/no-access", "/shared/abc123"]) {
      nav.pathname = pathname;
      const { unmount } = render(<UpdateNotice appName="Fleet" />);
      await act(async () => {});
      unmount();
    }
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("is the only deploy probe: the chat view no longer runs its own", () => {
    // Chat used to probe /api/version on the same events; two probes per event
    // would be pure waste now that this notice covers every page.
    const chat = readFileSync(path.resolve(__dirname, "../chat/ui/chat-experience.tsx"), "utf8");
    expect(chat).not.toContain('"/api/version"');
  });
});
