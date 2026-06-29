import { describe, expect, it } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { usePerConvComposerState } from "./usePerConvComposerState";
import type { PendingAttachment } from "./ChatChips";

// These tests pin the behavior-preserving contract of the per-conversation
// composer state extracted from ChatExperience in #401: key-scoped reads,
// dispatch-compatible setters that capture the key at render time, the
// delete-on-empty record optimization, and the atomic pending→real
// promotion. The component relied on all of these; the extraction must not
// drift from them.

const att = (clientId: string): PendingAttachment =>
  ({ clientId, name: `${clientId}.txt`, size: 1 }) as unknown as PendingAttachment;

describe("usePerConvComposerState", () => {
  it("defaults to empty values for an unseen key", () => {
    const { result } = renderHook(() => usePerConvComposerState("a"));
    expect(result.current.prompt).toBe("");
    expect(result.current.pendingAttachments).toEqual([]);
    expect(result.current.attachmentError).toBe(null);
    expect(result.current.isUploadingAttachments).toBe(false);
  });

  it("current-key setters write to (and clear) the active slot", () => {
    const { result } = renderHook(() => usePerConvComposerState("a"));

    act(() => result.current.setPrompt("hello"));
    expect(result.current.prompt).toBe("hello");

    // Functional updater form is supported (Dispatch compatibility).
    act(() => result.current.setPrompt((prev) => prev + " world"));
    expect(result.current.prompt).toBe("hello world");

    // Setting back to empty deletes the key from the backing record.
    act(() => result.current.setPrompt(""));
    expect(result.current.prompt).toBe("");
    expect("a" in result.current.promptByConv).toBe(false);
  });

  it("isolates state per conversation key", () => {
    const { result, rerender } = renderHook(
      ({ key }) => usePerConvComposerState(key),
      { initialProps: { key: "a" } },
    );
    act(() => result.current.setPrompt("draft-a"));

    rerender({ key: "b" });
    expect(result.current.prompt).toBe("");
    act(() => result.current.setPrompt("draft-b"));

    rerender({ key: "a" });
    expect(result.current.prompt).toBe("draft-a");
  });

  it("a setter from an older render still targets that render's key", () => {
    // This is the load-bearing closure-capture guarantee: an async submit
    // launched in conv A must keep writing to A even after the user has
    // navigated to B. The setter captures currentConvKey at render time.
    const { result, rerender } = renderHook(
      ({ key }) => usePerConvComposerState(key),
      { initialProps: { key: "a" } },
    );
    const setPromptFromA = result.current.setPrompt;

    rerender({ key: "b" });
    act(() => setPromptFromA("written-by-A's-setter"));

    expect(result.current.promptByConv.a).toBe("written-by-A's-setter");
    expect(result.current.prompt).toBe(""); // current key is "b", untouched
  });

  it("explicit-key setters and getPendingAttachmentsForKey target any slot", () => {
    const { result } = renderHook(() => usePerConvComposerState("a"));

    act(() => {
      result.current.setPromptForKey("other", "x");
      result.current.setPendingAttachmentsForKey("other", [att("f1")]);
      result.current.setAttachmentErrorForKey("other", "boom");
    });

    expect(result.current.promptByConv.other).toBe("x");
    expect(result.current.getPendingAttachmentsForKey("other")).toHaveLength(1);
    expect(result.current.attachmentErrorByConv.other).toBe("boom");

    // Clearing to the empty value removes the key (record stays sparse).
    act(() => {
      result.current.setPromptForKey("other", "");
      result.current.setPendingAttachmentsForKey("other", []);
      result.current.setAttachmentErrorForKey("other", null);
    });
    expect("other" in result.current.promptByConv).toBe(false);
    expect("other" in result.current.pendingAttachmentsByConv).toBe(false);
    expect("other" in result.current.attachmentErrorByConv).toBe(false);
  });

  it("markConvUploading / markConvUploadDone toggle the per-key flag", () => {
    const { result, rerender } = renderHook(
      ({ key }) => usePerConvComposerState(key),
      { initialProps: { key: "a" } },
    );
    act(() => result.current.markConvUploading("a"));
    expect(result.current.isUploadingAttachments).toBe(true);

    rerender({ key: "b" });
    expect(result.current.isUploadingAttachments).toBe(false); // scoped to "a"

    rerender({ key: "a" });
    act(() => result.current.markConvUploadDone("a"));
    expect(result.current.isUploadingAttachments).toBe(false);
  });

  it("promoteComposerKey moves every slot from the pending key to the real id", () => {
    const { result, rerender } = renderHook(
      ({ key }) => usePerConvComposerState(key),
      { initialProps: { key: "__pending__:1" } },
    );

    act(() => {
      result.current.setPrompt("carry me");
      result.current.setPendingAttachments([att("f1")]);
      result.current.setAttachmentError("err");
      result.current.markConvUploading("__pending__:1");
    });

    act(() => result.current.promoteComposerKey("__pending__:1", "real-id"));

    // Old key is fully drained.
    expect("__pending__:1" in result.current.promptByConv).toBe(false);
    expect("__pending__:1" in result.current.pendingAttachmentsByConv).toBe(false);
    expect("__pending__:1" in result.current.attachmentErrorByConv).toBe(false);

    // Values landed on the real id.
    rerender({ key: "real-id" });
    expect(result.current.prompt).toBe("carry me");
    expect(result.current.pendingAttachments).toHaveLength(1);
    expect(result.current.attachmentError).toBe("err");
    expect(result.current.isUploadingAttachments).toBe(true);
  });

  it("promoteComposerKey is a no-op for an empty draft (matches inline behavior)", () => {
    const { result } = renderHook(() => usePerConvComposerState("a"));
    act(() => result.current.promoteComposerKey("__pending__:9", "real"));
    expect(result.current.promptByConv).toEqual({});
    expect(result.current.pendingAttachmentsByConv).toEqual({});
    expect(result.current.attachmentErrorByConv).toEqual({});
  });
});
