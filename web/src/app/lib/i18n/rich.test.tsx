import { afterEach, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { setBundleLocalizations } from "./index";
import { rich } from "./rich";

afterEach(() => { cleanup(); setBundleLocalizations({ en: {} }); });

it("reorders rich slots without turning user text into markup or another slot", () => {
  setBundleLocalizations({ en: { "Owner {0}, link {1}": "Link {1}; owner {0}" } });
  const userText = "<img src=x onerror=alert(1)>{1}";
  const { container } = render(<p>{rich("Owner {0}, link {1}", userText, <a href="/settings">Settings</a>)}</p>);
  expect(container.textContent).toBe(`Link Settings; owner ${userText}`);
  expect(screen.getByRole("link", { name: "Settings" })).toHaveAttribute("href", "/settings");
  expect(container.querySelector("img")).toBeNull();
});
