import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { useRef } from "react";
import { FileUpload, type FileUploadHandle } from "./FileUpload";

// Component test for the React FileUpload (port of moc's file-upload.js). Covers
// add/remove, the 250MB validation reuse, and the imperative uploadAll() handle
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
    fireEvent.change(input, { target: { files: [makeFile("huge.bin", 300 * 1024 * 1024)] } });
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
});
