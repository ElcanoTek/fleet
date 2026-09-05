import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { DatasetsPanel } from "./DatasetsPanel";
import type { Dataset, DatasetRow } from "@/app/shared/lib/orchestratorApi";

// DatasetsPanel (#514) — the dataset tab strip, the per-dataset row table and
// the create modal. These pin three fixes: the row fetch is guarded against a
// slower, superseded response overwriting the current selection; the empty
// state waits for the first list response; and Delete asks through the app's
// ConfirmDialog rather than window.confirm.

const datasets = vi.fn();
const datasetRows = vi.fn();
const deleteDataset = vi.fn();
const createDataset = vi.fn();
vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    datasets: (...args: unknown[]) => datasets(...args),
    datasetRows: (...args: unknown[]) => datasetRows(...args),
    deleteDataset: (...args: unknown[]) => deleteDataset(...args),
    createDataset: (...args: unknown[]) => createDataset(...args),
  },
}));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function dataset(id: string, name: string): Dataset {
  return {
    id,
    name,
    goal: `goal for ${name}`,
    status: "idle",
    columns: [{ name: "company", type: "text" }],
  } as unknown as Dataset;
}

function row(id: string, company: string): DatasetRow {
  return { id, row_index: 0, status: "pending", cells: { company } } as unknown as DatasetRow;
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

describe("DatasetsPanel", () => {
  it("shows a loading line, not the empty state, until the first list response", async () => {
    const list = deferred<{ datasets: Dataset[] }>();
    datasets.mockReturnValue(list.promise);
    render(<DatasetsPanel />);
    expect(screen.getByTestId("datasets-loading")).toBeInTheDocument();
    expect(screen.queryByText(/No datasets yet/)).toBeNull();
    await act(async () => {
      list.resolve({ datasets: [] });
    });
    expect(screen.getByText(/No datasets yet/)).toBeInTheDocument();
    expect(screen.queryByTestId("datasets-loading")).toBeNull();
  });

  it("drops a superseded rows response so the slower first click cannot win", async () => {
    datasets.mockResolvedValue({ datasets: [dataset("a", "alpha"), dataset("b", "beta")] });
    const pending = new Map<string, ReturnType<typeof deferred<{ rows: DatasetRow[] }>>>();
    datasetRows.mockImplementation((id: string) => {
      const d = deferred<{ rows: DatasetRow[] }>();
      pending.set(id, d);
      return d.promise;
    });
    render(<DatasetsPanel />);

    fireEvent.click(await screen.findByRole("tab", { name: /alpha/ }));
    await waitFor(() => expect(pending.has("a")).toBe(true));
    // Rows for a selection still in flight render as loading, not "No rows".
    expect(screen.getByTestId("dataset-rows-loading")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("tab", { name: /beta/ }));
    await waitFor(() => expect(pending.has("b")).toBe(true));

    // Beta answers first, then alpha's slower reply arrives — and is ignored.
    await act(async () => {
      pending.get("b")!.resolve({ rows: [row("r-b", "Beta Corp")] });
    });
    expect(screen.getByText("Beta Corp")).toBeInTheDocument();
    await act(async () => {
      pending.get("a")!.resolve({ rows: [row("r-a", "Alpha Inc")] });
    });
    expect(screen.getByText("Beta Corp")).toBeInTheDocument();
    expect(screen.queryByText("Alpha Inc")).toBeNull();
  });

  it("clears the previous dataset's rows the moment another is selected", async () => {
    datasets.mockResolvedValue({ datasets: [dataset("a", "alpha"), dataset("b", "beta")] });
    const pending = new Map<string, ReturnType<typeof deferred<{ rows: DatasetRow[] }>>>();
    datasetRows.mockImplementation((id: string) => {
      const d = deferred<{ rows: DatasetRow[] }>();
      pending.set(id, d);
      return d.promise;
    });
    render(<DatasetsPanel />);
    fireEvent.click(await screen.findByRole("tab", { name: /alpha/ }));
    await waitFor(() => expect(pending.has("a")).toBe(true));
    await act(async () => {
      pending.get("a")!.resolve({ rows: [row("r-a", "Alpha Inc")] });
    });
    expect(screen.getByText("Alpha Inc")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("tab", { name: /beta/ }));
    // Alpha's rows are gone immediately — not left under beta's heading.
    expect(screen.queryByText("Alpha Inc")).toBeNull();
    expect(screen.getByTestId("dataset-rows-loading")).toBeInTheDocument();
  });

  it("confirms Delete with the app dialog, not window.confirm", async () => {
    datasets.mockResolvedValue({ datasets: [dataset("a", "alpha")] });
    datasetRows.mockResolvedValue({ rows: [], row_counts: {} });
    deleteDataset.mockResolvedValue({ status: "deleted" });
    const nativeConfirm = vi.spyOn(window, "confirm");
    render(<DatasetsPanel />);
    fireEvent.click(await screen.findByRole("tab", { name: /alpha/ }));
    await screen.findByRole("toolbar", { name: "Dataset actions" });

    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    const dialog = screen.getByRole("dialog", { name: "Delete dataset" });
    expect(dialog).toHaveTextContent(/Delete dataset "alpha" and all its rows/);
    expect(nativeConfirm).not.toHaveBeenCalled();
    expect(deleteDataset).not.toHaveBeenCalled();

    // Cancel keeps the dataset; confirming deletes it.
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    fireEvent.click(screen.getByRole("dialog").querySelector(".btn-primary")!);
    await waitFor(() => expect(deleteDataset).toHaveBeenCalledWith("a"));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("the create modal closes on Escape and starts focus in the Name field", async () => {
    datasets.mockResolvedValue({ datasets: [] });
    render(<DatasetsPanel />);
    fireEvent.click(await screen.findByRole("button", { name: "New dataset" }));
    const modal = screen.getByRole("dialog", { name: "New dataset" });
    expect(screen.getByPlaceholderText("prospect-leads")).toHaveFocus();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(modal).not.toBeInTheDocument();
  });
});
