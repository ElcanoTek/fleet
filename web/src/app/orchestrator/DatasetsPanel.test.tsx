import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { DatasetsPanel, ROWS_PAGE_SIZE } from "./DatasetsPanel";
import { ToastProvider } from "@/app/shared/ui/Toast";
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
    approveDatasetRows: (...args: unknown[]) => approveDatasetRows(...args),
  },
}));

const approveDatasetRows = vi.fn();

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

describe("DatasetsPanel rows paging", () => {
  it("requests an explicit page and says how many rows the filter covers", async () => {
    datasets.mockResolvedValue({ datasets: [dataset("a", "alpha")] });
    // 250 pending rows: more than one page at the server's 200 default, which
    // the panel used to request implicitly and then show as the whole table.
    datasetRows.mockResolvedValue({
      rows: Array.from({ length: ROWS_PAGE_SIZE }, (_, i) => ({ ...row(`r${i}`, `Co ${i}`), row_index: i })),
      row_counts: { pending: 250 },
    });
    render(<DatasetsPanel />);
    fireEvent.click(await screen.findByRole("tab", { name: /alpha/ }));

    await waitFor(() =>
      expect(screen.getByTestId("dataset-rows-showing")).toHaveTextContent(`Showing 1-${ROWS_PAGE_SIZE} of 250 rows`),
    );
    expect(datasetRows).toHaveBeenLastCalledWith("a", `?limit=${ROWS_PAGE_SIZE}&offset=0`);
    expect(screen.getByText("Page 1 of 2")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Next rows page" }));
    await waitFor(() =>
      expect(datasetRows).toHaveBeenLastCalledWith("a", `?limit=${ROWS_PAGE_SIZE}&offset=${ROWS_PAGE_SIZE}`),
    );

    // A filter change re-buckets the rows, so it snaps back to page 1.
    fireEvent.change(screen.getByLabelText("Filter rows by status"), { target: { value: "failed" } });
    await waitFor(() =>
      expect(datasetRows).toHaveBeenLastCalledWith("a", `?limit=${ROWS_PAGE_SIZE}&offset=0&status=failed`),
    );
  });
});

describe("DatasetsPanel rows paging shrink", () => {
  it("clamps the page when the row count drops under it", async () => {
    datasets.mockResolvedValue({ datasets: [dataset("a", "alpha")] });
    // 250 rows → 2 pages. Go to page 2, then let the count fall to a single
    // page (the last row of page 2 was approved or rerun out of the filter).
    datasetRows.mockResolvedValue({
      rows: Array.from({ length: ROWS_PAGE_SIZE }, (_, i) => ({ ...row(`r${i}`, `Co ${i}`), row_index: i })),
      // Some rows are still proposals, which is what enables Approve.
      row_counts: { pending: 200, proposed: 50 },
    });
    render(<DatasetsPanel />);
    fireEvent.click(await screen.findByRole("tab", { name: /alpha/ }));
    await waitFor(() => expect(screen.getByText("Page 1 of 2")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Next rows page" }));
    await waitFor(() =>
      expect(datasetRows).toHaveBeenLastCalledWith("a", `?limit=${ROWS_PAGE_SIZE}&offset=${ROWS_PAGE_SIZE}`),
    );

    // Approving refetches in place — the filter and dataset never change, so
    // the existing page-reset paths do not fire. This is the real trigger: the
    // count falls to one page's worth while the reader sits on page 2.
    approveDatasetRows.mockResolvedValue({ approved: 50 });
    datasetRows.mockResolvedValue({
      rows: Array.from({ length: ROWS_PAGE_SIZE }, (_, i) => ({ ...row(`r${i}`, `Co ${i}`), row_index: i })),
      row_counts: { pending: ROWS_PAGE_SIZE },
    });
    const callsBefore = datasetRows.mock.calls.length;
    fireEvent.click(screen.getByRole("button", { name: /Approve all proposed/ }));

    // Without the clamp the panel keeps offset 200 while the pager — which
    // renders only when there is more than one page — disappears, stranding
    // the reader on an empty table with no way back.
    await waitFor(() =>
      expect(datasetRows).toHaveBeenLastCalledWith("a", `?limit=${ROWS_PAGE_SIZE}&offset=0`),
    );
    // The panel settles on a page that exists, with rows on it — the pager
    // has already hidden itself by now (it needs more than one page), so a
    // reader left on the old offset would have no control to recover with.
    expect(datasetRows.mock.calls.length).toBeGreaterThan(callsBefore);
    expect(screen.queryByText(/Page 2 of/)).toBeNull();
    expect(screen.getByTestId("dataset-rows-showing")).toHaveTextContent(
      `Showing 1-${ROWS_PAGE_SIZE} of ${ROWS_PAGE_SIZE} rows`,
    );
  });
});

describe("DatasetsPanel Export CSV", () => {
  it("fetches the export and saves it instead of navigating", async () => {
    datasets.mockResolvedValue({ datasets: [dataset("a", "alpha")] });
    datasetRows.mockResolvedValue({ rows: [], row_counts: {} });
    const fetchSpy = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(new Response("company\n", { status: 200, headers: { "Content-Type": "text/csv" } }));
    // jsdom has no object URLs; the helper's save step needs both to exist.
    const createURL = vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:fleet/dataset");
    const revokeURL = vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
    const before = window.location.href;
    render(<DatasetsPanel />);
    fireEvent.click(await screen.findByRole("tab", { name: /alpha/ }));
    fireEvent.click(await screen.findByTestId("dataset-export-csv"));
    await waitFor(() => expect(fetchSpy).toHaveBeenCalled());
    expect(String(fetchSpy.mock.calls[0][0])).toBe("/api/orchestrator/datasets/a/export");
    expect(window.location.href).toBe(before);
    fetchSpy.mockRestore();
    createURL.mockRestore();
    revokeURL.mockRestore();
  });

  it("reports a failed export as a toast, in place", async () => {
    datasets.mockResolvedValue({ datasets: [dataset("a", "alpha")] });
    datasetRows.mockResolvedValue({ rows: [], row_counts: {} });
    const fetchSpy = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(new Response(JSON.stringify({ error: "session expired" }), { status: 401 }));
    const before = window.location.href;
    render(
      <ToastProvider>
        <DatasetsPanel />
      </ToastProvider>,
    );
    fireEvent.click(await screen.findByRole("tab", { name: /alpha/ }));
    fireEvent.click(await screen.findByTestId("dataset-export-csv"));
    expect(await screen.findByRole("alert")).toHaveTextContent("Export failed: session expired");
    expect(window.location.href).toBe(before);
    fetchSpy.mockRestore();
  });
});

describe("DatasetsPanel create modal columns", () => {
  it("keeps each column draft's inputs with it when the row above is removed", async () => {
    datasets.mockResolvedValue({ datasets: [] });
    render(<DatasetsPanel />);
    fireEvent.click(await screen.findByRole("button", { name: "New dataset" }));
    fireEvent.change(screen.getByLabelText("Column 1 name"), { target: { value: "first" } });
    fireEvent.change(screen.getByLabelText("Column 2 name"), { target: { value: "second" } });
    fireEvent.click(screen.getAllByRole("button", { name: "Remove column" })[0]);
    // The surviving draft is "second", now in slot 1 — with its own value.
    expect(screen.getByLabelText("Column 1 name")).toHaveValue("second");
    expect(screen.queryByLabelText("Column 2 name")).toBeNull();
  });
});
