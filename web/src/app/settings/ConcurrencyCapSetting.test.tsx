import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { ConcurrencyCapSetting } from "./ConcurrencyCapSetting";

const concurrency = vi.fn();
const setConcurrency = vi.fn();
vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    concurrency: (...args: unknown[]) => concurrency(...args),
    setConcurrency: (...args: unknown[]) => setConcurrency(...args),
  },
}));

// The v2-new global concurrency-cap setting (FLEET_MAX_CONCURRENT_AGENTS).

describe("ConcurrencyCapSetting", () => {
  beforeEach(() => {
    concurrency.mockReset();
    setConcurrency.mockReset();
    concurrency.mockResolvedValue({ max_concurrent_agents: 4, warm_pool_size: 2 });
    setConcurrency.mockResolvedValue({ max_concurrent_agents: 8, warm_pool_size: 2 });
  });
  afterEach(() => cleanup());

  it("loads and displays the current cap", async () => {
    render(<ConcurrencyCapSetting />);
    await waitFor(() => {
      expect((screen.getByLabelText("Global Concurrency Cap") as HTMLInputElement).value).toBe("4");
    });
  });

  it("saves a valid new cap via PUT /concurrency", async () => {
    render(<ConcurrencyCapSetting initialValue={4} />);
    const input = screen.getByLabelText("Global Concurrency Cap") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "8" } });
    fireEvent.click(screen.getByText("Save"));
    await waitFor(() => expect(setConcurrency).toHaveBeenCalledWith(8));
  });

  it("rejects an out-of-range cap before calling the API", async () => {
    render(<ConcurrencyCapSetting initialValue={4} />);
    const input = screen.getByLabelText("Global Concurrency Cap") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "999" } });
    fireEvent.click(screen.getByText("Save"));
    await waitFor(() => {
      expect(screen.getByTestId("concurrency-cap-error")).toBeInTheDocument();
    });
    expect(setConcurrency).not.toHaveBeenCalled();
  });

  it("rejects a fractional / non-integer cap before calling the API", async () => {
    render(<ConcurrencyCapSetting initialValue={4} />);
    const input = screen.getByLabelText("Global Concurrency Cap") as HTMLInputElement;
    // A number input won't hold "abc", but it will hold a non-integer string,
    // which validateConcurrencyCap rejects (must be a whole number).
    fireEvent.change(input, { target: { value: "4.5" } });
    fireEvent.click(screen.getByText("Save"));
    await waitFor(() => expect(screen.getByTestId("concurrency-cap-error")).toBeInTheDocument());
    expect(setConcurrency).not.toHaveBeenCalled();
  });
});
