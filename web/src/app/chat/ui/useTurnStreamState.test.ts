import { describe, expect, it } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { useTurnStreamState } from "./useTurnStreamState";

// Pins the behavior-preserving contract of the turn/SSE transport state
// extracted from ChatExperience in #401. The SSE loop reads these refs
// synchronously (not React state), so the mirror ref must update
// immediately and the pending→real promotion must re-key the abort
// controller, attached set, and streaming set as one synchronous unit.

describe("useTurnStreamState", () => {
  it("markConvStreaming updates both the state set and the mirror ref synchronously", () => {
    const { result } = renderHook(() => useTurnStreamState("a"));
    act(() => result.current.markConvStreaming("a"));
    expect(result.current.streamingConvs.has("a")).toBe(true);
    expect(result.current.streamingConvsRef.current.has("a")).toBe(true);
    expect(result.current.isStreaming).toBe(true);
  });

  it("isStreaming is derived for the current key only", () => {
    const { result, rerender } = renderHook(
      ({ key }) => useTurnStreamState(key),
      { initialProps: { key: "a" } },
    );
    act(() => result.current.markConvStreaming("a"));
    expect(result.current.isStreaming).toBe(true);

    rerender({ key: "b" });
    expect(result.current.isStreaming).toBe(false); // "b" is idle
    expect(result.current.streamingConvs.has("a")).toBe(true); // "a" still running
  });

  it("markConvIdle removes the key from state and mirror", () => {
    const { result } = renderHook(() => useTurnStreamState("a"));
    act(() => result.current.markConvStreaming("a"));
    act(() => result.current.markConvIdle("a"));
    expect(result.current.streamingConvs.has("a")).toBe(false);
    expect(result.current.streamingConvsRef.current.has("a")).toBe(false);
  });

  it("promoteStreamKey re-keys the abort controller, attached set, and streaming set", () => {
    const { result, rerender } = renderHook(
      ({ key }) => useTurnStreamState(key),
      { initialProps: { key: "__pending__:1" } },
    );

    const controller = new AbortController();
    act(() => {
      result.current.abortControllersRef.current["__pending__:1"] = controller;
      result.current.attachedConvIdsRef.current.add("__pending__:1");
      result.current.markConvStreaming("__pending__:1");
    });

    act(() => result.current.promoteStreamKey("__pending__:1", "real-id"));

    // Abort-controller IDENTITY is preserved under the new key.
    expect(result.current.abortControllersRef.current["real-id"]).toBe(controller);
    expect("__pending__:1" in result.current.abortControllersRef.current).toBe(false);

    // Attached set follows.
    expect(result.current.attachedConvIdsRef.current.has("real-id")).toBe(true);
    expect(result.current.attachedConvIdsRef.current.has("__pending__:1")).toBe(false);

    // Streaming set (state + mirror) follows.
    rerender({ key: "real-id" });
    expect(result.current.isStreaming).toBe(true);
    expect(result.current.streamingConvsRef.current.has("real-id")).toBe(true);
    expect(result.current.streamingConvsRef.current.has("__pending__:1")).toBe(false);
  });

  it("promoteStreamKey only re-keys handles that exist (no phantom entries)", () => {
    const { result } = renderHook(() => useTurnStreamState("a"));
    // Nothing registered under the old key → nothing should appear under new.
    act(() => result.current.promoteStreamKey("__pending__:7", "real"));
    expect("real" in result.current.abortControllersRef.current).toBe(false);
    expect(result.current.attachedConvIdsRef.current.has("real")).toBe(false);
    expect(result.current.streamingConvsRef.current.has("real")).toBe(false);
  });

  it("exposes the SSE dedup ref maps as stable mutable handles", () => {
    const { result, rerender } = renderHook(() => useTurnStreamState("a"));
    const lastEventIdRef = result.current.lastEventIdByConvRef;
    const turnIdRef = result.current.currentTurnIdByConvRef;
    const reattachRef = result.current.reattachInFlightRef;

    act(() => {
      result.current.lastEventIdByConvRef.current["a"] = 5;
      result.current.currentTurnIdByConvRef.current["a"] = "t1";
      result.current.reattachInFlightRef.current.add("a");
    });

    rerender();
    // Ref identity is stable across renders (refs, not recreated state).
    expect(result.current.lastEventIdByConvRef).toBe(lastEventIdRef);
    expect(result.current.currentTurnIdByConvRef).toBe(turnIdRef);
    expect(result.current.reattachInFlightRef).toBe(reattachRef);
    expect(result.current.lastEventIdByConvRef.current["a"]).toBe(5);
    expect(result.current.currentTurnIdByConvRef.current["a"]).toBe("t1");
    expect(result.current.reattachInFlightRef.current.has("a")).toBe(true);
  });
});
