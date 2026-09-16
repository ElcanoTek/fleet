import { afterEach, describe, expect, it, vi } from "vitest";
import { signOut, signOutAfter } from "./signOut";

describe("signOut", () => {
  afterEach(() => {
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  it("submits a top-level POST form to /api/auth/logout", () => {
    const submit = vi
      .spyOn(HTMLFormElement.prototype, "submit")
      .mockImplementation(() => {});
    signOut();
    const form = document.querySelector("form");
    expect(form).not.toBeNull();
    expect(form!.method).toBe("post");
    expect(new URL(form!.action, "https://fleet.example").pathname).toBe(
      "/api/auth/logout",
    );
    expect(submit).toHaveBeenCalledTimes(1);
  });

  it("signOutAfter signs out once the cleanup settles, whether it resolved or rejected", async () => {
    const submit = vi
      .spyOn(HTMLFormElement.prototype, "submit")
      .mockImplementation(() => {});
    await signOutAfter(Promise.resolve("ok"));
    await signOutAfter(Promise.reject(new Error("orchestrator down")));
    expect(submit).toHaveBeenCalledTimes(2);
  });

  it("signOutAfter does not let a hung cleanup hold the sign-out", async () => {
    vi.useFakeTimers();
    try {
      const submit = vi
        .spyOn(HTMLFormElement.prototype, "submit")
        .mockImplementation(() => {});
      const never = new Promise<void>(() => {});
      const done = signOutAfter(never, 1500);
      expect(submit).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(1500);
      await done;
      expect(submit).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });
});
