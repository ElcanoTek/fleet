import { afterEach, describe, expect, it, vi } from "vitest";
import { renderHook, waitFor, act, cleanup } from "@testing-library/react";
import { useCancellableFetch } from "./useCancellableFetch";

// Unit tests for the shared cancellable-fetch hook — the one audited place a
// setState-after-await happens. We exercise the happy path, error capture, the
// cancelled-ref guard on unmount mid-flight, and the `enabled` gate.

// A deferred promise so a test can hold a fetch in-flight and resolve/reject it
// at a controlled moment.
function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("useCancellableFetch", () => {
  it("starts loading then resolves to data", async () => {
    const fetcher = vi.fn().mockResolvedValue({ value: 42 });
    const { result } = renderHook(() => useCancellableFetch(fetcher, []));

    // Lazy-initialized to enabled=true, so it starts loading before any fetch.
    expect(result.current.loading).toBe(true);
    expect(result.current.data).toBeNull();
    expect(result.current.error).toBeNull();

    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.data).toEqual({ value: 42 });
    expect(result.current.error).toBeNull();
    expect(fetcher).toHaveBeenCalledTimes(1);
  });

  it("captures a fetch error into `error` and clears loading", async () => {
    const fetcher = vi.fn().mockRejectedValue(new Error("boom"));
    const { result } = renderHook(() => useCancellableFetch(fetcher, []));

    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.error).toBe("boom");
    expect(result.current.data).toBeNull();
  });

  it("does not setState after the component unmounts mid-flight (cancelled-ref guard)", async () => {
    const d = deferred<{ ok: boolean }>();
    const fetcher = vi.fn().mockReturnValue(d.promise);
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});

    const { result, unmount } = renderHook(() => useCancellableFetch(fetcher, []));
    // Let the deferred microtask kick off the fetch.
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
    expect(result.current.loading).toBe(true);

    // Unmount while the fetch is still pending, then resolve it.
    unmount();
    await act(async () => {
      d.resolve({ ok: true });
      await d.promise;
    });

    // The guard must have dropped the post-unmount setData/setLoading. React
    // logs an "update on unmounted component" error if a guard is missing.
    expect(errorSpy).not.toHaveBeenCalledWith(
      expect.stringContaining("unmounted"),
    );
  });

  it("does not fetch while disabled and is not loading", async () => {
    const fetcher = vi.fn().mockResolvedValue({ value: 1 });
    const { result, rerender } = renderHook(
      ({ enabled }: { enabled: boolean }) =>
        useCancellableFetch(fetcher, [], { enabled }),
      { initialProps: { enabled: false } },
    );

    // Lazy-init to enabled=false → not loading, and the disabled branch settles
    // any residual loading without fetching.
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(fetcher).not.toHaveBeenCalled();
    expect(result.current.data).toBeNull();

    // Flipping enabled true triggers the fetch.
    rerender({ enabled: true });
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.data).toEqual({ value: 1 });
  });

  it("reload() re-runs the fetcher and refreshes data", async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValueOnce({ n: 1 })
      .mockResolvedValueOnce({ n: 2 });
    const { result } = renderHook(() => useCancellableFetch(fetcher, []));

    await waitFor(() => expect(result.current.data).toEqual({ n: 1 }));

    await act(async () => {
      await result.current.reload();
    });
    expect(fetcher).toHaveBeenCalledTimes(2);
    expect(result.current.data).toEqual({ n: 2 });
  });

  it("refetches when a dependency changes", async () => {
    const fetcher = vi.fn().mockResolvedValue({ ok: true });
    const { rerender } = renderHook(
      ({ id }: { id: number }) => useCancellableFetch(() => fetcher(id), [id]),
      { initialProps: { id: 1 } },
    );

    await waitFor(() => expect(fetcher).toHaveBeenCalledWith(1));
    rerender({ id: 2 });
    await waitFor(() => expect(fetcher).toHaveBeenCalledWith(2));
  });
});
