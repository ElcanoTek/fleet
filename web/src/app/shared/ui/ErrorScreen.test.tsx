import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import RouteError from "@/app/error";
import { ErrorScreen } from "./ErrorScreen";

// The crash screen must always offer a way out (a Home Screen web app has no
// reload button of its own), and must reload by itself only for a failed
// chunk load, and only once.

describe("ErrorScreen", () => {
  const originalLocation = window.location;
  let reload: ReturnType<typeof vi.fn>;
  let assign: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    reload = vi.fn();
    assign = vi.fn();
    Object.defineProperty(window, "location", {
      configurable: true,
      value: { ...originalLocation, reload, assign },
    });
    vi.spyOn(console, "error").mockImplementation(() => {});
  });

  afterEach(() => {
    Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
    window.sessionStorage.clear();
    vi.restoreAllMocks();
  });

  it("offers a reload and a way home, and shows the server's reference", () => {
    const error = Object.assign(new Error("An error occurred in the Server Components render."), {
      digest: "1234567890",
    });
    render(<RouteError error={error} />);
    expect(screen.getByRole("alert")).toHaveTextContent("Something went wrong");
    expect(screen.getByText("Reference: 1234567890")).toBeInTheDocument();
    expect(reload).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    expect(reload).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Go to the home page" }));
    expect(assign).toHaveBeenCalledWith("/");
  });

  it("reloads by itself once for a failed chunk load, then waits for the reader", () => {
    const chunkError = new Error("Failed to load chunk /_next/static/chunks/old.js");
    chunkError.name = "ChunkLoadError";
    const first = render(<ErrorScreen error={chunkError} />);
    expect(reload).toHaveBeenCalledTimes(1);
    first.unmount();

    // The reload landed on the same failure: no second automatic reload.
    render(<ErrorScreen error={chunkError} />);
    expect(reload).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("button", { name: "Reload" })).toBeInTheDocument();
  });

  it("does not reload by itself for any other error", () => {
    render(<ErrorScreen error={new TypeError("Cannot read properties of undefined")} />);
    expect(reload).not.toHaveBeenCalled();
  });
});
