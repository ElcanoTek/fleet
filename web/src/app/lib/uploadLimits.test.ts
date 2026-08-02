import { describe, expect, it } from "vitest";
import {
  DEFAULT_UPLOAD_MAX_BYTES,
  LARGE_UPLOAD_WARNING_BYTES,
  formatBytes,
  largeUploadWarning,
  screenFilesForUpload,
} from "./uploadLimits";

const MB = 1024 * 1024;

describe("screenFilesForUpload", () => {
  it("accepts files at or under the cap", () => {
    const files = [
      { name: "a.pdf", size: 10 * MB },
      { name: "b.zip", size: DEFAULT_UPLOAD_MAX_BYTES },
    ];
    const out = screenFilesForUpload(files, DEFAULT_UPLOAD_MAX_BYTES);
    expect(out.accepted).toHaveLength(2);
    expect(out.rejected).toHaveLength(0);
    expect(out.error).toBeNull();
  });

  it("rejects an oversize file with a message naming it and both sizes", () => {
    const files = [{ name: "big.mov", size: 1.5 * 1024 * MB }];
    const out = screenFilesForUpload(files, DEFAULT_UPLOAD_MAX_BYTES);
    expect(out.accepted).toHaveLength(0);
    expect(out.rejected).toHaveLength(1);
    expect(out.error).toContain('"big.mov"');
    expect(out.error).toContain("1.5 GB");
    expect(out.error).toContain("1.0 GB per-file upload limit");
  });

  it("keeps the files that fit when a multi-select mixes sizes", () => {
    const files = [
      { name: "ok.csv", size: 2 * MB },
      { name: "huge1.bin", size: 2 * 1024 * MB },
      { name: "huge2.bin", size: 3 * 1024 * MB },
    ];
    const out = screenFilesForUpload(files, DEFAULT_UPLOAD_MAX_BYTES);
    expect(out.accepted.map((f) => f.name)).toEqual(["ok.csv"]);
    expect(out.rejected).toHaveLength(2);
    expect(out.error).toContain("2 files");
    expect(out.error).toContain('"huge1.bin"');
    expect(out.error).toContain('"huge2.bin"');
  });

  it("respects a server-supplied cap different from the default", () => {
    const files = [{ name: "clip.mp4", size: 30 * MB }];
    const out = screenFilesForUpload(files, 25 * MB);
    expect(out.rejected).toHaveLength(1);
    expect(out.error).toContain("25.0 MB per-file upload limit");
  });
});

describe("largeUploadWarning", () => {
  it("is silent below the threshold", () => {
    expect(largeUploadWarning(LARGE_UPLOAD_WARNING_BYTES - 1)).toBeNull();
    expect(largeUploadWarning(0)).toBeNull();
  });

  it("warns at and above the threshold with the total size", () => {
    const msg = largeUploadWarning(500 * MB);
    expect(msg).toContain("500.0 MB");
    expect(msg).toContain("take a while");
  });
});

describe("formatBytes", () => {
  it("formats each magnitude", () => {
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(2048)).toBe("2.0 KB");
    expect(formatBytes(5 * MB)).toBe("5.0 MB");
    expect(formatBytes(1536 * MB)).toBe("1.5 GB");
  });
});
