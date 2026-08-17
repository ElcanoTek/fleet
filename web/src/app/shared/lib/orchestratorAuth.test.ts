import { afterEach, describe, expect, it } from "vitest";
import { purgeLegacyOrchestratorTokens } from "./orchestratorAuth";

afterEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
});

describe("purgeLegacyOrchestratorTokens", () => {
  it("removes leftover moc bearer keys and leaves other prefs alone", () => {
    window.localStorage.setItem("orchestratorToken", "secret-bearer");
    window.localStorage.setItem("userToken", "legacy-moc");
    window.localStorage.setItem("chat-theme-preference", "dark");
    window.sessionStorage.setItem("orchestratorToken", "session-leftover");

    purgeLegacyOrchestratorTokens();

    expect(window.localStorage.getItem("orchestratorToken")).toBeNull();
    expect(window.localStorage.getItem("userToken")).toBeNull();
    expect(window.localStorage.getItem("chat-theme-preference")).toBe("dark");
    expect(window.sessionStorage.getItem("orchestratorToken")).toBeNull();
  });

  it("is a no-op when nothing is stored", () => {
    expect(() => purgeLegacyOrchestratorTokens()).not.toThrow();
  });
});
