import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup, act } from "@testing-library/react";
import { useRef } from "react";
import { FileUpload, type FileUploadHandle } from "./FileUpload";

// Component test for the React FileUpload (port of moc's file-upload.js). Covers
// add/remove, upload validation, and the imperative uploadAll() handle
// the task form drives at submit.

function makeFile(name: string, size: number, type = "text/plain"): File {
  const f = new File(["x"], name, { type });
  Object.defineProperty(f, "size", { value: size });
  return f;
}

function Harness({ onHandle }: { onHandle?: (h: FileUploadHandle) => void }) {
  const ref = useRef<FileUploadHandle | null>(null);
  return (
    <FileUpload
      registerHandle={(h) => {
        ref.current = h;
        onHandle?.(h);
      }}
    />
  );
}

describe("FileUpload", () => {
  afterEach(() => cleanup());

  it("lists a valid added file", async () => {
    const { container } = render(<Harness />);
    const input = container.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [makeFile("report.csv", 1024)] } });
    await waitFor(() => expect(screen.getByText("report.csv")).toBeInTheDocument());
  });

  it("flags an oversized file as invalid (reusing validateFile)", async () => {
    const { container } = render(<Harness />);
    const input = container.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [makeFile("huge.bin", 2 * 1024 * 1024 * 1024)] } });
    await waitFor(() => {
      expect(screen.getByText(/File size exceeds maximum allowed size/)).toBeInTheDocument();
    });
  });

  it("removes a file via the remove button", async () => {
    const { container } = render(<Harness />);
    const input = container.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [makeFile("a.txt", 10)] } });
    await waitFor(() => screen.getByText("a.txt"));
    fireEvent.click(screen.getByLabelText("Remove a.txt"));
    await waitFor(() => expect(screen.queryByText("a.txt")).not.toBeInTheDocument());
  });

  it("uploadAll() drives the uploader and returns server filenames", async () => {
    let handle: FileUploadHandle | null = null;
    const { container } = render(<Harness onHandle={(h) => (handle = h)} />);
    const input = container.querySelector('input[type="file"]') as HTMLInputElement;
    fireEvent.change(input, { target: { files: [makeFile("data.json", 50)] } });
    await waitFor(() => screen.getByText("data.json"));

    const uploader = vi.fn().mockResolvedValue({ filename: "stored-data.json" });
    const names = await handle!.uploadAll(uploader);
    expect(uploader).toHaveBeenCalledTimes(1);
    expect(names).toEqual(["stored-data.json"]);
  });

  it("names the overflow visibly when a batch exceeds the 10-file cap (adds what fits)", async () => {
    const { container } = render(<Harness />);
    const input = container.querySelector('input[type="file"]') as HTMLInputElement;
    const twelve = Array.from({ length: 12 }, (_, i) => makeFile(`f${i}.txt`, 10));
    fireEvent.change(input, { target: { files: twelve } });
    await waitFor(() => screen.getByText("f9.txt"));
    // The first 10 landed; the 2 over the cap are named in an adjacent error.
    expect(screen.queryByText("f10.txt")).not.toBeInTheDocument();
    expect(
      screen.getByText("Up to 10 files per task — 2 files were not added."),
    ).toBeInTheDocument();
    // Removing a file clears the batch error.
    fireEvent.click(screen.getByLabelText("Remove f0.txt"));
    await waitFor(() =>
      expect(screen.queryByText(/were not added/)).not.toBeInTheDocument(),
    );
  });

  it("handle.addFiles() accepts files forwarded from the dialog's drop target", async () => {
    let handle: FileUploadHandle | null = null;
    render(<Harness onHandle={(h) => (handle = h)} />);
    handle!.addFiles([makeFile("dropped.csv", 20)]);
    await waitFor(() => expect(screen.getByText("dropped.csv")).toBeInTheDocument());
    expect(handle!.hasFiles()).toBe(true);
  });

  it("retries failed uploads without dropping attachments or reuploading successes", async () => {
    let handle: FileUploadHandle | null = null;
    render(<Harness onHandle={(h) => (handle = h)} />);
    act(() => handle!.addFiles([makeFile("first.txt", 10), makeFile("second.txt", 10)]));
    const uploader = vi.fn()
      .mockResolvedValueOnce({ filename: "stored-first.txt" })
      .mockRejectedValueOnce(new Error("Upload interrupted"))
      .mockResolvedValueOnce({ filename: "stored-second.txt" });

    await act(async () => {
      await expect(handle!.uploadAll(uploader)).rejects.toThrow("Upload interrupted");
    });
    expect(handle!.hasFiles()).toBe(true);
    expect(screen.getByText("Upload interrupted")).toBeInTheDocument();
    await act(async () => {
      expect(await handle!.uploadAll(uploader)).toEqual(["stored-first.txt", "stored-second.txt"]);
    });
    expect(uploader).toHaveBeenCalledTimes(3);
    expect(uploader.mock.calls[2][0].name).toBe("second.txt");
    expect(screen.queryByText("Upload interrupted")).not.toBeInTheDocument();
  });

  it("keeps a lone failed attachment eligible for the next submit", async () => {
    let handle: FileUploadHandle | null = null;
    render(<Harness onHandle={(h) => (handle = h)} />);
    act(() => handle!.addFiles([makeFile("only.txt", 10)]));
    await act(async () => {
      await expect(handle!.uploadAll(vi.fn().mockRejectedValue(new Error("Offline"))))
        .rejects.toThrow("Offline");
    });
    expect(handle!.hasFiles()).toBe(true);
  });

  it("deduplicates within a drop and counts only valid unique files toward the cap", () => {
    let handle: FileUploadHandle | null = null;
    render(<Harness onHandle={(h) => (handle = h)} />);
    const first = makeFile("first.txt", 10);
    const remaining = Array.from({ length: 9 }, (_, i) => makeFile(`next${i}.txt`, 10));
    act(() => handle!.addFiles([
      first, first, makeFile("too-large.bin", 2 * 1024 * 1024 * 1024), ...remaining,
    ]));
    expect(screen.getAllByText("first.txt")).toHaveLength(1);
    expect(screen.getByText("next8.txt")).toBeInTheDocument();
    expect(screen.queryByText(/not added/)).not.toBeInTheDocument();
  });
});
