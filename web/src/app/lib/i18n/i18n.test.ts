import { afterEach, describe, expect, it, vi } from "vitest";
import es from "./es.json";
import pt from "./pt.json";
import { detectLanguage, LANGUAGE_KEY, languageURL, rememberLanguage, supportedLanguage, translate } from ".";

afterEach(() => {
  vi.restoreAllMocks();
  localStorage.clear();
  document.cookie = "elcano_language=; Path=/; Max-Age=0";
  window.history.replaceState({}, "", "/");
});

describe("language selection", () => {
  it("normalizes regional browser languages and rejects unsupported languages", () => {
    expect(supportedLanguage("es-MX")).toBe("es");
    expect(supportedLanguage("pt-BR")).toBe("pt");
    expect(supportedLanguage("en-GB")).toBe("en");
    expect(supportedLanguage("fr")).toBeNull();
    expect(supportedLanguage(null)).toBeNull();
  });

  it("prefers an explicit URL, then a saved choice, then the browser", () => {
    vi.spyOn(navigator, "language", "get").mockReturnValue("pt-BR");
    expect(detectLanguage()).toBe("pt");
    localStorage.setItem(LANGUAGE_KEY, "es");
    expect(detectLanguage()).toBe("es");
    window.history.replaceState({}, "", "/settings?lang=en");
    expect(detectLanguage()).toBe("en");
    window.history.replaceState({}, "", "/settings?lang=unsupported");
    expect(detectLanguage()).toBe("es");
  });

  it("works when storage is unavailable and keeps existing navigation state", () => {
    vi.spyOn(window, "localStorage", "get").mockImplementation(() => { throw new Error("blocked"); });
    window.history.replaceState({}, "", "/settings?lang=pt");
    expect(detectLanguage()).toBe("pt");
    expect(languageURL("es", "https://app.example/settings?tab=general#language"))
      .toBe("https://app.example/settings?tab=general&lang=es#language");
  });

  it("keeps the selected language across sign-in redirects when localStorage is denied", () => {
    vi.spyOn(window, "localStorage", "get").mockImplementation(() => { throw new Error("blocked"); });
    rememberLanguage("es");
    expect(document.cookie).toContain("elcano_language=es");
    window.history.replaceState({}, "", "/chat");
    expect(detectLanguage()).toBe("es");
  });
});

describe("translations", () => {
  it("preserves unknown text and interpolates values without reinterpreting them", () => {
    expect(translate("es", "Save")).toBe("Guardar");
    expect(translate("pt", "Cancel")).toBe("Cancelar");
    expect(translate("es", "User-owned content")).toBe("User-owned content");
    expect(translate("pt", "Chat {0}", ["<script>{1}</script>"]))
      .toBe("Chat <script>{1}</script>");
    expect(translate("en", "Chat {0}", [42])).toBe("Chat 42");
  });

  it("has matching catalogs with every interpolation preserved", () => {
    expect(Object.keys(es).sort()).toEqual(Object.keys(pt).sort());
    const placeholders = (text: string) => [...text.matchAll(/\{\d+\}/g)].map((m) => m[0]).sort();
    for (const catalog of [es, pt]) {
      for (const [source, message] of Object.entries(catalog)) {
        expect(message.trim(), source).not.toBe("");
        expect(placeholders(message), source).toEqual(placeholders(source));
      }
    }
  });
});
