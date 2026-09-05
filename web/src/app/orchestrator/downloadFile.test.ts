import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { downloadFile } from "./downloadFile";

// downloadFile is the fetch-first replacement for `window.location.href =
// <csv endpoint>`: a non-2xx must surface as an error the caller can show,
// never as a navigation away from the dashboard.

const fetchMock = vi.fn();
let clicked: HTMLAnchorElement[] = [];

beforeEach(() => {
  clicked = [];
  vi.stubGlobal("fetch", fetchMock);
  // jsdom implements neither object URLs nor anchor navigation.
  URL.createObjectURL = vi.fn(() => "blob:fleet/report");
  URL.revokeObjectURL = vi.fn();
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
    clicked.push(this);
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("downloadFile", () => {
  it("clicks a download anchor named by Content-Disposition on success", async () => {
    fetchMock.mockResolvedValue(
      new Response("a,b\n1,2\n", {
        status: 200,
        headers: {
          "content-type": "text/csv",
          "content-disposition": 'attachment; filename="usage-2026-06.csv"',
        },
      }),
    );
    await downloadFile("/api/orchestrator/admin/usage?format=csv", "usage.csv");
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/orchestrator/admin/usage?format=csv",
      expect.objectContaining({ credentials: "same-origin" }),
    );
    expect(clicked).toHaveLength(1);
    expect(clicked[0].download).toBe("usage-2026-06.csv");
    expect(clicked[0].href).toContain("blob:fleet/report");
    expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:fleet/report");
    // The throwaway anchor does not linger in the document.
    expect(document.body.querySelector("a[download]")).toBeNull();
  });

  it("falls back to the caller's filename without a disposition header", async () => {
    fetchMock.mockResolvedValue(new Response("a,b\n", { status: 200 }));
    await downloadFile("/x.csv", "fallback.csv");
    expect(clicked[0].download).toBe("fallback.csv");
  });

  it("throws the API error on a non-2xx and never clicks", async () => {
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ error: "session expired" }), {
        status: 401,
        statusText: "Unauthorized",
        headers: { "content-type": "application/json" },
      }),
    );
    await expect(downloadFile("/x.csv", "x.csv")).rejects.toThrow("session expired");
    expect(clicked).toHaveLength(0);
    expect(URL.createObjectURL).not.toHaveBeenCalled();
  });

  it("falls back to the status line when the error body is not JSON", async () => {
    fetchMock.mockResolvedValue(
      new Response("<html>Bad Gateway</html>", { status: 502, statusText: "Bad Gateway" }),
    );
    await expect(downloadFile("/x.csv", "x.csv")).rejects.toThrow("HTTP 502 Bad Gateway");
  });
});
