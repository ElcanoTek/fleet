import { render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

// The root layout's key check runs on whatever page the person opens, except
// the signed-out ones: there is no session there to renew with.

const { ensureCurrentPushKey } = vi.hoisted(() => ({ ensureCurrentPushKey: vi.fn(async () => "none") }));
vi.mock("./pushKey", () => ({ ensureCurrentPushKey }));

describe("PushKeyRenewal", () => {
  afterEach(() => {
    ensureCurrentPushKey.mockClear();
    window.history.replaceState(null, "", "/");
  });

  it("checks the key on a signed-in page", async () => {
    const { PushKeyRenewal } = await import("./PushKeyRenewal");
    for (const path of ["/chat", "/orchestrator", "/settings"]) {
      ensureCurrentPushKey.mockClear();
      window.history.replaceState(null, "", path);
      render(<PushKeyRenewal />);
      expect(ensureCurrentPushKey, path).toHaveBeenCalledTimes(1);
    }
  });

  it("skips the sign-in page and shared links", async () => {
    const { PushKeyRenewal } = await import("./PushKeyRenewal");
    for (const path of ["/login", "/shared/abc"]) {
      window.history.replaceState(null, "", path);
      render(<PushKeyRenewal />);
    }
    expect(ensureCurrentPushKey).not.toHaveBeenCalled();
  });
});
